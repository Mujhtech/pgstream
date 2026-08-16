package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"github.com/mujhtech/pgstream/config"
	"github.com/mujhtech/pgstream/internal/storage"
	"github.com/spf13/cobra"
)

// RegisterStatusCommand shows migration progress from the session store. It
// reads checkpoints only, so it can watch a session that another process
// (an interactive `session` run or the server) is driving right now.
func RegisterStatusCommand() *cobra.Command {

	var id string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show progress of migration sessions",
		Long: `Reads per-table checkpoints from the session store and prints progress.
Without --id it lists every session; with --id it shows per-table detail.
Progress updates after every committed batch, so this also observes a
migration currently running in another terminal or in the server.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := godotenv.Load(config.DefaultConfigFilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("load %s: %w", config.DefaultConfigFilePath, err)
			}
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("load configuration: %w", err)
			}
			metadataStorage, err := storage.InitStorage(cmd.Context(), cfg.Database)
			if err != nil {
				return fmt.Errorf("open session storage: %w", err)
			}
			defer metadataStorage.Close()

			if id == "" {
				return printSessionList(cmd.Context(), metadataStorage)
			}
			return printSessionDetail(cmd.Context(), metadataStorage, id)
		},
	}

	cmd.Flags().StringVar(&id, "id", "", "session id to inspect in detail")
	return cmd
}

func printSessionList(ctx context.Context, store *storage.Storage) error {
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	for _, session := range sessions {
		records, err := store.ListMigrationsBySession(ctx, session.Id)
		if err != nil {
			return err
		}
		var rowsCopied, rowsTotal int64
		done, failed, inProgress := 0, 0, 0
		for _, record := range records {
			rowsCopied += record.LastOffset
			rowsTotal += record.RowCount
			switch record.Status {
			case "done":
				done++
			case "error":
				failed++
			case "in_progress":
				inProgress++
			}
		}
		status := "created"
		switch {
		case failed > 0:
			status = "error"
		case inProgress > 0:
			status = "in_progress"
		case len(records) > 0 && done == len(records):
			status = "done"
		}
		fmt.Printf("%s  %s  tables %d/%d done  rows %d/~%d  started %s\n",
			session.Id, status, done, len(records), rowsCopied, rowsTotal,
			session.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	fmt.Println("\nInspect one with: pgstream status --id <session-id>")
	return nil
}

func printSessionDetail(ctx context.Context, store *storage.Storage, id string) error {
	session, err := store.GetSessionByID(ctx, id)
	if err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("session %q was not found", id)
	}
	records, err := store.ListMigrationsBySession(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("Session %s (started %s)\n\n", id, session.CreatedAt.Format("2006-01-02 15:04:05"))
	if len(records) == 0 {
		fmt.Println("No table progress recorded yet.")
		return nil
	}
	for _, record := range records {
		progress := fmt.Sprintf("%d rows", record.LastOffset)
		if record.RowCount > 0 {
			progress = fmt.Sprintf("%d/~%d rows", record.LastOffset, record.RowCount)
		}
		line := fmt.Sprintf("  %-40s %-12s %s", record.TableName, record.Status, progress)
		if record.ErrorMessage != "" {
			line += "  " + record.ErrorMessage
		}
		fmt.Println(line)
	}

	if err := printRecentEvents(ctx, store, id); err != nil {
		return err
	}
	fmt.Println("\nProgress checkpoints update after every committed batch.")
	return nil
}

// printRecentEvents tails the persisted engine log for the session.
func printRecentEvents(ctx context.Context, store *storage.Storage, id string) error {
	const tail = 15
	last, err := store.LastSessionEvent(ctx, id)
	if err != nil {
		return err
	}
	if last == nil {
		return nil
	}
	afterID := last.Id - tail
	if afterID < 0 {
		afterID = 0
	}
	events, err := store.ListSessionEventsAfter(ctx, id, afterID, tail)
	if err != nil {
		return err
	}
	fmt.Println("\nRecent log:")
	for _, event := range events {
		fmt.Printf("  %s  %s\n", event.CreatedAt.Format("15:04:05"), event.Message)
	}
	return nil
}
