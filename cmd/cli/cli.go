package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/charmbracelet/lipgloss"
	"github.com/joho/godotenv"
	"github.com/mujhtech/pgstream/config"
	"github.com/mujhtech/pgstream/internal/cmd/views/session"
	"github.com/mujhtech/pgstream/internal/database"
	"github.com/mujhtech/pgstream/internal/encrypt"
	"github.com/mujhtech/pgstream/internal/migrator"
	"github.com/mujhtech/pgstream/internal/storage"
	"github.com/mujhtech/pgstream/internal/utils/views"
	"github.com/spf13/cobra"
)

func RegisterCliCommand() *cobra.Command {

	var (
		id        string
		batchSize int
	)

	cmd := &cobra.Command{
		Use:   "session",
		Short: "Start pgstream session",
		Long:  ``,
		RunE: func(cmd *cobra.Command, args []string) error {
			if batchSize <= 0 {
				return fmt.Errorf("batch size must be greater than zero")
			}
			if err := godotenv.Load(config.DefaultConfigFilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("load %s: %w", config.DefaultConfigFilePath, err)
			}
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("load configuration: %w", err)
			}
			sessionCipher, err := encrypt.NewAesGcm(cfg.EncryptionKey)
			if err != nil {
				return fmt.Errorf("configure session credential encryption from ENCRYPTION_KEY: %w", err)
			}
			metadataStorage, err := storage.InitStorage(cmd.Context(), cfg.Database)
			if err != nil {
				return fmt.Errorf("initialize session storage: %w", err)
			}
			defer metadataStorage.Close()

			return cliSession(cmd.Context(), metadataStorage, sessionCipher, id, batchSize)
		},
	}

	cmd.Flags().StringVar(&id, "id", "", "session id")
	cmd.Flags().IntVar(&batchSize, "batch-size", 5000, "rows read from MySQL per batch")

	return cmd

}

func cliSession(ctx context.Context, storage *storage.Storage, sessionCipher encrypt.Encrypt, id string, batchSize int) error {

	var inputs migrator.Config
	var err error
	var sessionId string
	freshSession := id == ""

	if id == "" {

		inputs, err = session.Run()

		if err != nil {
			if errors.Is(err, session.ErrCancelled) {
				return nil
			}
			return err
		}

		inputBytes, err := json.Marshal(inputs)

		if err != nil {
			return err
		}

		encryptedConnector, err := sessionCipher.Encrypt(inputBytes)
		if err != nil {
			return fmt.Errorf("encrypt session connector: %w", err)
		}
		createdID, err := storage.CreateSession(ctx, encryptedConnector)
		if err != nil {
			return err
		}
		sessionId = createdID

	} else {

		session, err := storage.GetSessionByID(ctx, id)

		if err != nil {
			return err
		}
		if session == nil {
			return fmt.Errorf("session %q was not found", id)
		}

		connectorJSON := session.Connector
		if encrypt.IsEncryptedCiphertext(connectorJSON) {
			connectorJSON, err = sessionCipher.Decrypt(connectorJSON)
			if err != nil {
				return fmt.Errorf("decrypt connector for session %q: %w", id, err)
			}
		} else {
			if !json.Valid([]byte(connectorJSON)) {
				return fmt.Errorf("session %q contains an invalid legacy connector", id)
			}
			encryptedConnector, encryptErr := sessionCipher.Encrypt([]byte(connectorJSON))
			if encryptErr != nil {
				return fmt.Errorf("encrypt legacy connector for session %q: %w", id, encryptErr)
			}
			if updateErr := storage.UpdateSessionConnector(ctx, session.Id, encryptedConnector); updateErr != nil {
				return fmt.Errorf("secure legacy connector for session %q: %w", id, updateErr)
			}
		}

		err = json.Unmarshal([]byte(connectorJSON), &inputs)

		if err != nil {
			return err
		}

		sessionId = session.Id
	}

	if inputs.MySQL == nil || inputs.PostgreSQL == nil {
		return fmt.Errorf("session %q is missing MySQL or PostgreSQL connector configuration", sessionId)
	}
	fmt.Printf("Session ID: %s (resume with --id %s)\n", sessionId, sessionId)

	var mysqlConnector *database.Database
	var postgresConnector *database.Database

	err = views.WithInlineSpinner(fmt.Sprintf("Connecting to MySQL %s:**********@%s:%s/%s...", inputs.MySQL.User, inputs.MySQL.Host, inputs.MySQL.Port, inputs.MySQL.Database), func() error {

		port, err := parseConnectorPort(inputs.MySQL.Port, "MySQL")
		if err != nil {
			return err
		}

		mysqlConnector, err = database.Connect(ctx, config.Database{
			Driver:   config.DatabaseDriverMySQL,
			Host:     inputs.MySQL.Host,
			Port:     port,
			User:     inputs.MySQL.User,
			Password: inputs.MySQL.Password,
			Database: inputs.MySQL.Database,
		})

		if err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return err
	}
	defer mysqlConnector.Close()

	fmt.Println(lipgloss.Place(0, 0, 0, 0, views.BasicLayout.Render("✅ Connected to MySQL")))

	err = views.WithInlineSpinner(fmt.Sprintf("Connecting to PostgreSQL %s:**********@%s:%s/%s...", inputs.PostgreSQL.User, inputs.PostgreSQL.Host, inputs.PostgreSQL.Port, inputs.PostgreSQL.Database), func() error {

		port, err := parseConnectorPort(inputs.PostgreSQL.Port, "PostgreSQL")
		if err != nil {
			return err
		}

		postgresConnector, err = database.Connect(ctx, config.Database{
			Driver:   config.DatabaseDriverPostgres,
			Host:     inputs.PostgreSQL.Host,
			Port:     port,
			User:     inputs.PostgreSQL.User,
			Schema:   inputs.PostgreSQL.Schema,
			Password: inputs.PostgreSQL.Password,
			Database: inputs.PostgreSQL.Database,
			Options:  "sslmode=prefer&connect_timeout=30",
		})

		if err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return err
	}
	defer postgresConnector.Close()

	fmt.Println(lipgloss.Place(0, 0, 0, 0, views.BasicLayout.Render("✅ Connected to PostgreSQL")))

	migrator, err := migrator.New(
		mysqlConnector,
		postgresConnector,
		storage,
		sessionId,
		migrator.WithBatchSize(batchSize),
		migrator.WithFreshSession(freshSession),
	)

	if err != nil {
		return err
	}

	err = migrator.Start(ctx)

	if err != nil {
		return err
	}

	return nil
}

func parseConnectorPort(value string, databaseName string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s port must be a number: %w", databaseName, err)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s port must be between 1 and 65535", databaseName)
	}
	return port, nil
}
