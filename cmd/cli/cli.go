package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

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

var hintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#767676"))

type sessionOptions struct {
	id                string
	batchSize         int
	includeTables     []string
	excludeTables     []string
	dryRun            bool
	loadMethod        string
	workers           int
	skipSnapshotLock  bool
	sourceCompression bool
	casts             []string
}

func RegisterCliCommand() *cobra.Command {

	var opts sessionOptions

	cmd := &cobra.Command{
		Use:   "session",
		Short: "Start pgstream session",
		Long:  ``,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.batchSize <= 0 {
				return fmt.Errorf("batch size must be greater than zero")
			}
			if err := godotenv.Load(config.DefaultConfigFilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("load %s: %w", config.DefaultConfigFilePath, err)
			}
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("load configuration: %w", err)
			}

			// A dry run without --id never touches session storage, so it
			// works without an encryption key or metadata database.
			needsSessionStore := !opts.dryRun || opts.id != ""

			var sessionCipher encrypt.Encrypt
			var metadataStorage *storage.Storage
			if needsSessionStore {
				sessionCipher, err = encrypt.NewAesGcm(cfg.EncryptionKey)
				if err != nil {
					return fmt.Errorf("configure session credential encryption from ENCRYPTION_KEY: %w", err)
				}
				metadataStorage, err = storage.InitStorage(cmd.Context(), cfg.Database)
				if err != nil {
					return fmt.Errorf("initialize session storage: %w", err)
				}
				defer metadataStorage.Close()
			}

			return cliSession(cmd.Context(), metadataStorage, sessionCipher, opts)
		},
	}

	cmd.Flags().StringVar(&opts.id, "id", "", "session id")
	cmd.Flags().IntVar(&opts.batchSize, "batch-size", 5000, "rows read from MySQL per batch")
	cmd.Flags().StringSliceVar(&opts.includeTables, "include-tables", nil, "migrate only these source tables (comma-separated)")
	cmd.Flags().StringSliceVar(&opts.excludeTables, "exclude-tables", nil, "migrate all source tables except these (comma-separated)")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "report the schema translation plan without writing to PostgreSQL or session storage")
	cmd.Flags().StringVar(&opts.loadMethod, "load-method", string(migrator.LoadMethodCopy), "how batches are written to PostgreSQL: copy or insert")
	cmd.Flags().IntVar(&opts.workers, "workers", 1, "tables migrated concurrently; workers > 1 aligns per-worker snapshots under a brief FLUSH TABLES WITH READ LOCK")
	cmd.Flags().BoolVar(&opts.skipSnapshotLock, "skip-snapshot-lock", false, "skip the snapshot alignment lock; only safe when the source receives no writes during the migration")
	cmd.Flags().StringArrayVar(&opts.casts, "cast", nil, "type-mapping override, repeatable: 'table.column=TYPE' or 'mysqltype=TYPE' (e.g. --cast 'jobs.id=text' --cast 'datetime=timestamptz')")
	cmd.Flags().BoolVar(&opts.sourceCompression, "source-compression", true, "zlib-compress the MySQL connection; a large win for remote sources, disable with --source-compression=false")
	cmd.MarkFlagsMutuallyExclusive("include-tables", "exclude-tables")

	return cmd

}

func cliSession(ctx context.Context, storage *storage.Storage, sessionCipher encrypt.Encrypt, opts sessionOptions) error {

	var inputs migrator.Config
	var err error
	var sessionId string
	id := opts.id
	freshSession := id == ""

	if id == "" {

		inputs, err = session.Run()

		if err != nil {
			if errors.Is(err, session.ErrCancelled) {
				return nil
			}
			return err
		}

		if opts.dryRun {
			sessionId = "dry-run"
		} else {
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
		}

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
			if !opts.dryRun {
				encryptedConnector, encryptErr := sessionCipher.Encrypt([]byte(connectorJSON))
				if encryptErr != nil {
					return fmt.Errorf("encrypt legacy connector for session %q: %w", id, encryptErr)
				}
				if updateErr := storage.UpdateSessionConnector(ctx, session.Id, encryptedConnector); updateErr != nil {
					return fmt.Errorf("secure legacy connector for session %q: %w", id, updateErr)
				}
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
	if !opts.dryRun {
		fmt.Printf("Session ID: %s\n", sessionId)
		fmt.Println(hintStyle.Render(fmt.Sprintf("  resume anytime: pgstream session --id %s", sessionId)))
	}

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
			Options:  mysqlOptions(opts.sourceCompression),
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

	fmt.Printf("✅ Connected to MySQL      %s@%s:%s/%s\n", inputs.MySQL.User, inputs.MySQL.Host, inputs.MySQL.Port, inputs.MySQL.Database)

	err = views.WithInlineSpinner(fmt.Sprintf("Connecting to PostgreSQL %s:**********@%s:%s/%s...", inputs.PostgreSQL.User, inputs.PostgreSQL.Host, inputs.PostgreSQL.Port, inputs.PostgreSQL.Database), func() error {

		port, err := parseConnectorPort(inputs.PostgreSQL.Port, "PostgreSQL")
		if err != nil {
			return err
		}

		postgresConnector, err = database.ConnectPostgresPreferSSL(ctx, config.Database{
			Driver:   config.DatabaseDriverPostgres,
			Host:     inputs.PostgreSQL.Host,
			Port:     port,
			User:     inputs.PostgreSQL.User,
			Schema:   inputs.PostgreSQL.Schema,
			Password: inputs.PostgreSQL.Password,
			Database: inputs.PostgreSQL.Database,
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

	schemaNote := inputs.PostgreSQL.Schema
	if schemaNote == "" {
		schemaNote = "public"
	}
	fmt.Printf("✅ Connected to PostgreSQL %s@%s:%s/%s (schema %s)\n", inputs.PostgreSQL.User, inputs.PostgreSQL.Host, inputs.PostgreSQL.Port, inputs.PostgreSQL.Database, schemaNote)
	fmt.Println()

	engineOptions := []migrator.Option{
		migrator.WithBatchSize(opts.batchSize),
		migrator.WithFreshSession(freshSession),
		migrator.WithTableFilter(opts.includeTables, opts.excludeTables),
		migrator.WithLoadMethod(migrator.LoadMethod(opts.loadMethod)),
		migrator.WithWorkers(opts.workers),
		migrator.WithSkipSnapshotLock(opts.skipSnapshotLock),
		migrator.WithCastRules(opts.casts),
	}

	// Persist engine events so the server, web UI, and `pgstream status`
	// can follow this CLI-driven migration live. Persistence is best-effort:
	// a failing metadata write must not break the migration.
	var recordEvent func(event migrator.Event)
	if !opts.dryRun {
		var warnOnce sync.Once
		// A cancelled root context (Ctrl+C) must not prevent recording the
		// final events observers rely on.
		eventCtx := context.WithoutCancel(ctx)
		recordEvent = func(event migrator.Event) {
			if _, err := storage.AppendSessionEvent(eventCtx, sessionEventFromEngine(sessionId, event)); err != nil {
				warnOnce.Do(func() {
					fmt.Fprintf(os.Stderr, "warning: failed to record session events for observers: %v\n", err)
				})
			}
		}
		engineOptions = append(engineOptions, migrator.WithEventSink(func(event migrator.Event) {
			fmt.Println(event.Message)
			recordEvent(event)
		}))
	}

	engine, err := migrator.New(
		mysqlConnector,
		postgresConnector,
		storage,
		sessionId,
		engineOptions...,
	)

	if err != nil {
		return err
	}

	if opts.dryRun {
		report, err := engine.DryRun(ctx)
		if err != nil {
			return err
		}
		report.Render(func(line string) { fmt.Println(line) })
		if report.IssueCount > 0 {
			return fmt.Errorf("dry run found %d blocking issues", report.IssueCount)
		}
		return nil
	}

	if err := engine.Start(ctx); err != nil {
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			message := fmt.Sprintf("⏸️  Migration interrupted; progress is checkpointed. Resume with: pgstream session --id %s", sessionId)
			fmt.Println(message)
			recordEvent(migrator.Event{
				Time:    time.Now(),
				Level:   migrator.EventWarn,
				Message: message,
			})
			return fmt.Errorf("migration interrupted")
		}
		// Failures outside a table's data copy would otherwise be invisible
		// to observers reading the session store.
		recordEvent(migrator.Event{
			Time:    time.Now(),
			Level:   migrator.EventError,
			Message: fmt.Sprintf("❌ Migration failed: %v", err),
		})
		return err
	}
	return nil
}

func sessionEventFromEngine(sessionId string, event migrator.Event) storage.SessionEvent {
	return storage.SessionEvent{
		SessionId:     sessionId,
		Level:         string(event.Level),
		Message:       event.Message,
		TableName:     event.Table,
		ProcessedRows: event.ProcessedRows,
		TotalRows:     event.TotalRows,
		CreatedAt:     event.Time,
	}
}

// mysqlOptions builds source DSN options. Wire compression trades idle CPU
// for a large transfer reduction on text-heavy tables, which dominates
// migration time when the source is remote.
func mysqlOptions(compression bool) string {
	if compression {
		return "compress=true"
	}
	return ""
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
