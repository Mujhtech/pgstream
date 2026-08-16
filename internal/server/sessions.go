package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mujhtech/pgstream/internal/migrator"
	"github.com/mujhtech/pgstream/internal/storage"
)

// sessionSummary is the list-view projection of a session: identity, derived
// status, and aggregate progress. The stored connector (credentials) is never
// exposed.
type sessionSummary struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	TablesTotal int       `json:"tables_total"`
	TablesDone  int       `json:"tables_done"`
	RowsCopied  int64     `json:"rows_copied"`
	RowsTotal   int64     `json:"rows_total"`
	LastUpdate  time.Time `json:"last_update"`
	// External marks sessions with no run in this server process — for
	// example a migration driven by the CLI. Progress still updates from
	// the shared session store.
	External bool `json:"external"`
}

func (s *Server) summarizeSession(ctx context.Context, session storage.Session) (sessionSummary, error) {
	summary := sessionSummary{
		ID:        session.Id,
		CreatedAt: session.CreatedAt,
		Status:    "created",
		External:  true,
	}

	records, err := s.storage.ListMigrationsBySession(ctx, session.Id)
	if err != nil {
		return summary, err
	}
	summary.TablesTotal = len(records)
	anyError, anyInProgress := false, false
	for _, record := range records {
		summary.RowsCopied += record.LastOffset
		summary.RowsTotal += record.RowCount
		if record.UpdatedAt.After(summary.LastUpdate) {
			summary.LastUpdate = record.UpdatedAt
		}
		switch record.Status {
		case "done":
			summary.TablesDone++
		case "error":
			anyError = true
			if summary.Error == "" {
				summary.Error = fmt.Sprintf("%s: %s", record.TableName, record.ErrorMessage)
			}
		case "in_progress":
			anyInProgress = true
		}
	}
	switch {
	case anyError:
		summary.Status = "error"
	case anyInProgress:
		summary.Status = "in_progress"
	case len(records) > 0 && summary.TablesDone == len(records):
		summary.Status = "done"
	}

	// Persisted engine events cover phases the per-table records cannot see:
	// schema creation before any checkpoint exists, index/FK work after all
	// tables are done, and failures outside a table's data copy.
	if lastEvent, err := s.storage.LastSessionEvent(ctx, session.Id); err == nil && lastEvent != nil {
		if lastEvent.CreatedAt.After(summary.LastUpdate) {
			summary.LastUpdate = lastEvent.CreatedAt
		}
		switch {
		case strings.HasPrefix(lastEvent.Message, "✅ Migration completed"):
			summary.Status = "done"
		case lastEvent.Level == string(migrator.EventError):
			summary.Status = "error"
			if summary.Error == "" {
				summary.Error = lastEvent.Message
			}
		case summary.Status == "created":
			summary.Status = "in_progress"
		}
	}

	// A live run in this process is the freshest source of truth; once it
	// finishes, the persisted records and events are authoritative so an
	// external resume (e.g. the CLI) is reflected correctly.
	if activeRun := s.runs.get(session.Id); activeRun != nil {
		if runState, runErr := activeRun.snapshot(); runState == runStatusRunning {
			summary.External = false
			summary.Status = string(runState)
			if runErr != nil {
				summary.Error = runErr.Error()
			}
		}
	}
	return summary, nil
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.storage.ListSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	summaries := make([]sessionSummary, 0, len(sessions))
	for _, session := range sessions {
		summary, err := s.summarizeSession(r.Context(), session)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		summaries = append(summaries, summary)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": summaries})
}

// streamExternalSession serves the events endpoint for a session with no run
// in this process by tailing the persisted engine events in the shared
// session store. This is how a CLI-driven migration's log becomes watchable
// in the web UI.
func (s *Server) streamExternalSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	record, err := s.storage.GetSessionByID(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if record == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("session %q was not found", sessionID))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming is not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// The banner carries no SSE id so it never disturbs Last-Event-ID.
	writeBanner := func(message string) bool {
		payload, err := json.Marshal(sseEvent{
			ID:      -1,
			Time:    time.Now(),
			Level:   migrator.EventInfo,
			Message: message,
		})
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	writeStored := func(event storage.SessionEvent) bool {
		payload, err := json.Marshal(sseEvent{
			ID:            event.Id,
			Time:          event.CreatedAt,
			Level:         migrator.EventLevel(event.Level),
			Message:       event.Message,
			Table:         event.TableName,
			ProcessedRows: event.ProcessedRows,
			TotalRows:     event.TotalRows,
		})
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Id, payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	var lastID int64
	if lastEventID := r.Header.Get("Last-Event-ID"); lastEventID != "" {
		if parsed, err := strconv.ParseInt(lastEventID, 10, 64); err == nil && parsed > 0 {
			lastID = parsed
		}
	}

	if !writeBanner(fmt.Sprintf("👀 Watching session %s from the session store; it runs in another process (e.g. the CLI) and its log streams here.", sessionID)) {
		return
	}

	emit := func() (bool, bool) {
		wrote := false
		for {
			events, err := s.storage.ListSessionEventsAfter(r.Context(), sessionID, lastID, 500)
			if err != nil {
				return writeBanner(fmt.Sprintf("⚠️  Failed to read session events: %v", err)), wrote
			}
			for _, event := range events {
				if !writeStored(event) {
					return false, wrote
				}
				lastID = event.Id
				wrote = true
			}
			if len(events) < 500 {
				return true, wrote
			}
		}
	}

	ok, wrote := emit()
	if !ok {
		return
	}
	if !wrote && lastID == 0 {
		if !writeBanner("No events recorded yet — the owning process may not have started, or it predates event persistence. Progress will appear as soon as it logs.") {
			return
		}
	}

	poll := time.NewTicker(700 * time.Millisecond)
	defer poll.Stop()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
			if ok, _ := emit(); !ok {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
