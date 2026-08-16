package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mujhtech/pgstream/config"
	"github.com/mujhtech/pgstream/internal/database"
	"github.com/mujhtech/pgstream/internal/encrypt"
	"github.com/mujhtech/pgstream/internal/migrator"
	"github.com/mujhtech/pgstream/internal/storage"
	"github.com/mujhtech/pgstream/webui"
	"github.com/rs/zerolog"
)

// Server exposes the migration engine over HTTP for the bundled web UI.
type Server struct {
	cfg     *config.Config
	storage *storage.Storage
	cipher  encrypt.Encrypt
	runs    *runRegistry
	rootCtx context.Context
	logger  zerolog.Logger
	// runWG tracks in-flight migration goroutines so shutdown waits for
	// them to unwind (checkpoint, record the interrupt event) before the
	// process exits.
	runWG sync.WaitGroup
}

func New(rootCtx context.Context, cfg *config.Config, store *storage.Storage, cipher encrypt.Encrypt) *Server {
	return &Server{
		cfg:     cfg,
		storage: store,
		cipher:  cipher,
		runs:    newRunRegistry(),
		rootCtx: rootCtx,
		logger:  *zerolog.Ctx(rootCtx),
	}
}

type connectorRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
	Schema   string `json:"schema,omitempty"`
}

type sessionRequest struct {
	MySQL            *connectorRequest `json:"mysql"`
	Postgres         *connectorRequest `json:"postgres"`
	BatchSize        int               `json:"batch_size,omitempty"`
	IncludeTables    []string          `json:"include_tables,omitempty"`
	ExcludeTables    []string          `json:"exclude_tables,omitempty"`
	LoadMethod       string            `json:"load_method,omitempty"`
	Workers          int               `json:"workers,omitempty"`
	SkipSnapshotLock bool              `json:"skip_snapshot_lock,omitempty"`
}

type resumeRequest struct {
	BatchSize        int      `json:"batch_size,omitempty"`
	IncludeTables    []string `json:"include_tables,omitempty"`
	ExcludeTables    []string `json:"exclude_tables,omitempty"`
	LoadMethod       string   `json:"load_method,omitempty"`
	Workers          int      `json:"workers,omitempty"`
	SkipSnapshotLock bool     `json:"skip_snapshot_lock,omitempty"`
}

type runOptions struct {
	batchSize        int
	includeTables    []string
	excludeTables    []string
	loadMethod       string
	workers          int
	skipSnapshotLock bool
	fresh            bool
}

func (r *sessionRequest) validate() error {
	if r.MySQL == nil || r.Postgres == nil {
		return fmt.Errorf("mysql and postgres connection settings are required")
	}
	for name, connector := range map[string]*connectorRequest{"mysql": r.MySQL, "postgres": r.Postgres} {
		if strings.TrimSpace(connector.Host) == "" {
			return fmt.Errorf("%s host is required", name)
		}
		if connector.Port < 1 || connector.Port > 65535 {
			return fmt.Errorf("%s port must be between 1 and 65535", name)
		}
		if strings.TrimSpace(connector.User) == "" {
			return fmt.Errorf("%s user is required", name)
		}
		if strings.TrimSpace(connector.Database) == "" {
			return fmt.Errorf("%s database is required", name)
		}
	}
	return nil
}

func (r *sessionRequest) migratorConfig() migrator.Config {
	return migrator.Config{
		MySQL: &migrator.MySQLConfig{
			Host:     r.MySQL.Host,
			Port:     strconv.Itoa(r.MySQL.Port),
			User:     r.MySQL.User,
			Password: r.MySQL.Password,
			Database: r.MySQL.Database,
		},
		PostgreSQL: &migrator.PostgreSQLConfig{
			Host:     r.Postgres.Host,
			Port:     strconv.Itoa(r.Postgres.Port),
			User:     r.Postgres.User,
			Password: r.Postgres.Password,
			Database: r.Postgres.Database,
			Schema:   r.Postgres.Schema,
		},
	}
}

// Handler builds the HTTP routes: the JSON API plus the embedded web UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/dry-run", s.handleDryRun)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSessionStatus)
	mux.HandleFunc("POST /api/sessions/{id}/start", s.handleResumeSession)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleSessionEvents)

	s.mountWebUI(mux)

	return s.withCORS(mux)
}

// ListenAndServe blocks until the context is cancelled, then shuts down
// gracefully.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Request contexts derive from the server context, so long-lived
		// SSE streams unwind on shutdown instead of stalling it.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.ListenAndServe()
	}()
	s.logger.Info().Str("addr", addr).Msg("pgstream server listening")

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			// Stragglers (e.g. a client that never drains its stream) do
			// not warrant a scary exit error; close them and leave quietly.
			s.logger.Warn().Err(err).Msg("graceful shutdown timed out; closing remaining connections")
			_ = httpServer.Close()
		}

		// Give interrupted migrations a moment to checkpoint and record
		// their final events before the process exits.
		runsDone := make(chan struct{})
		go func() {
			s.runWG.Wait()
			close(runsDone)
		}()
		select {
		case <-runsDone:
		case <-time.After(15 * time.Second):
			s.logger.Warn().Msg("timed out waiting for interrupted migrations to unwind")
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(s.cfg.Cors.AllowedOrigins))
	for _, origin := range s.cfg.Cors.AllowedOrigins {
		allowed[origin] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if _, ok := allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) mountWebUI(mux *http.ServeMux) {
	dist, err := fs.Sub(webui.Dist, "dist")
	if err != nil {
		s.logger.Warn().Err(err).Msg("embedded web UI unavailable")
		return
	}
	if _, err := fs.Stat(dist, "index.html"); err != nil {
		// The binary was built without running the web UI build first.
		s.logger.Warn().Msg("web UI assets were not embedded; run `make webui` before building the binary")
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "The web UI was not embedded in this binary. Build it with `make webui` and rebuild pgstream. The JSON API under /api is fully functional.", http.StatusNotFound)
		})
		return
	}
	fileServer := http.FileServer(http.FS(dist))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requested := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if requested != "" {
			if _, err := fs.Stat(dist, requested); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		// Single-page app fallback.
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var request sessionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if err := request.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	inputs := request.migratorConfig()
	inputBytes, err := json.Marshal(inputs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	encryptedConnector, err := s.cipher.Encrypt(inputBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("encrypt session connector: %w", err))
		return
	}
	sessionID, err := s.storage.CreateSession(r.Context(), encryptedConnector)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("create session: %w", err))
		return
	}

	options := runOptions{
		batchSize:        request.BatchSize,
		includeTables:    request.IncludeTables,
		excludeTables:    request.ExcludeTables,
		loadMethod:       request.LoadMethod,
		workers:          request.Workers,
		skipSnapshotLock: request.SkipSnapshotLock,
		fresh:            true,
	}
	if err := s.startRun(sessionID, inputs, options); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": sessionID})
}

func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	inputs, err := s.loadSessionConfig(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	// The body is optional for resumes.
	var request resumeRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read request body: %w", err))
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &request); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
			return
		}
	}

	options := runOptions{
		batchSize:        request.BatchSize,
		includeTables:    request.IncludeTables,
		excludeTables:    request.ExcludeTables,
		loadMethod:       request.LoadMethod,
		workers:          request.Workers,
		skipSnapshotLock: request.SkipSnapshotLock,
		fresh:            false,
	}
	if err := s.startRun(sessionID, inputs, options); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": sessionID})
}

func (s *Server) loadSessionConfig(ctx context.Context, sessionID string) (migrator.Config, error) {
	var inputs migrator.Config
	record, err := s.storage.GetSessionByID(ctx, sessionID)
	if err != nil {
		return inputs, err
	}
	if record == nil {
		return inputs, fmt.Errorf("session %q was not found", sessionID)
	}
	connectorJSON := record.Connector
	if encrypt.IsEncryptedCiphertext(connectorJSON) {
		connectorJSON, err = s.cipher.Decrypt(connectorJSON)
		if err != nil {
			return inputs, fmt.Errorf("decrypt connector for session %q: %w", sessionID, err)
		}
	} else if !json.Valid([]byte(connectorJSON)) {
		return inputs, fmt.Errorf("session %q contains an invalid legacy connector", sessionID)
	}
	if err := json.Unmarshal([]byte(connectorJSON), &inputs); err != nil {
		return inputs, err
	}
	if inputs.MySQL == nil || inputs.PostgreSQL == nil {
		return inputs, fmt.Errorf("session %q is missing MySQL or PostgreSQL connector configuration", sessionID)
	}
	return inputs, nil
}

func (s *Server) startRun(sessionID string, inputs migrator.Config, options runOptions) error {
	runCtx, cancel := context.WithCancel(s.rootCtx)
	activeRun, err := s.runs.begin(sessionID, cancel)
	if err != nil {
		cancel()
		return err
	}

	// Events persist first — the store's row id is the shared SSE id across
	// the live hub and the storage tail — then fan out to live subscribers.
	// If persistence fails the event still streams, under a synthetic
	// negative id that clients display without deduplication.
	var syntheticID atomic.Int64
	syntheticID.Store(-1)
	sink := func(event migrator.Event) {
		id, err := s.storage.AppendSessionEvent(context.Background(), storage.SessionEvent{
			SessionId:     sessionID,
			Level:         string(event.Level),
			Message:       event.Message,
			TableName:     event.Table,
			ProcessedRows: event.ProcessedRows,
			TotalRows:     event.TotalRows,
			CreatedAt:     event.Time,
		})
		if err != nil {
			s.logger.Warn().Err(err).Str("session", sessionID).Msg("failed to persist session event")
			id = syntheticID.Add(-1)
		}
		activeRun.publish(id, event)
	}

	s.runWG.Add(1)
	go func() {
		defer s.runWG.Done()
		defer cancel()
		err := s.executeMigration(runCtx, sessionID, inputs, options, sink)
		// The terminal event must be published before finish, because finish
		// closes every live subscriber channel.
		if err != nil {
			if errors.Is(err, context.Canceled) || runCtx.Err() != nil {
				sink(migrator.Event{
					Time:    time.Now(),
					Level:   migrator.EventWarn,
					Message: fmt.Sprintf("⏸️  Migration interrupted; progress is checkpointed. Resume session %s to continue.", sessionID),
				})
			} else {
				sink(migrator.Event{
					Time:    time.Now(),
					Level:   migrator.EventError,
					Message: fmt.Sprintf("❌ Migration failed: %v", err),
				})
			}
		}
		activeRun.finish(err)
		if err != nil {
			s.logger.Error().Err(err).Str("session", sessionID).Msg("migration stopped")
			return
		}
		s.logger.Info().Str("session", sessionID).Msg("migration finished")
	}()
	return nil
}

func (s *Server) executeMigration(ctx context.Context, sessionID string, inputs migrator.Config, options runOptions, sink migrator.EventSink) error {
	mysqlPort, err := strconv.Atoi(inputs.MySQL.Port)
	if err != nil {
		return fmt.Errorf("MySQL port must be a number: %w", err)
	}
	postgresPort, err := strconv.Atoi(inputs.PostgreSQL.Port)
	if err != nil {
		return fmt.Errorf("PostgreSQL port must be a number: %w", err)
	}

	sink(migrator.Event{Time: time.Now(), Level: migrator.EventInfo, Message: fmt.Sprintf("🔌 Connecting to MySQL %s@%s:%d/%s...", inputs.MySQL.User, inputs.MySQL.Host, mysqlPort, inputs.MySQL.Database)})
	mysqlConnector, err := database.Connect(ctx, config.Database{
		Driver:   config.DatabaseDriverMySQL,
		Host:     inputs.MySQL.Host,
		Port:     mysqlPort,
		User:     inputs.MySQL.User,
		Password: inputs.MySQL.Password,
		Database: inputs.MySQL.Database,
	})
	if err != nil {
		return fmt.Errorf("connect to MySQL: %w", err)
	}
	defer mysqlConnector.Close()

	sink(migrator.Event{Time: time.Now(), Level: migrator.EventInfo, Message: fmt.Sprintf("🔌 Connecting to PostgreSQL %s@%s:%d/%s...", inputs.PostgreSQL.User, inputs.PostgreSQL.Host, postgresPort, inputs.PostgreSQL.Database)})
	postgresConnector, err := database.ConnectPostgresPreferSSL(ctx, config.Database{
		Driver:   config.DatabaseDriverPostgres,
		Host:     inputs.PostgreSQL.Host,
		Port:     postgresPort,
		User:     inputs.PostgreSQL.User,
		Schema:   inputs.PostgreSQL.Schema,
		Password: inputs.PostgreSQL.Password,
		Database: inputs.PostgreSQL.Database,
	})
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer postgresConnector.Close()

	migratorOptions := []migrator.Option{
		migrator.WithFreshSession(options.fresh),
		migrator.WithEventSink(sink),
		migrator.WithTableFilter(options.includeTables, options.excludeTables),
		migrator.WithSkipSnapshotLock(options.skipSnapshotLock),
	}
	if options.workers > 0 {
		migratorOptions = append(migratorOptions, migrator.WithWorkers(options.workers))
	}
	if options.batchSize > 0 {
		migratorOptions = append(migratorOptions, migrator.WithBatchSize(options.batchSize))
	}
	if options.loadMethod != "" {
		migratorOptions = append(migratorOptions, migrator.WithLoadMethod(migrator.LoadMethod(options.loadMethod)))
	}

	engine, err := migrator.New(mysqlConnector, postgresConnector, s.storage, sessionID, migratorOptions...)
	if err != nil {
		return err
	}
	return engine.Start(ctx)
}

func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	var request sessionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if err := request.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	inputs := request.migratorConfig()

	mysqlConnector, err := database.Connect(r.Context(), config.Database{
		Driver:   config.DatabaseDriverMySQL,
		Host:     inputs.MySQL.Host,
		Port:     request.MySQL.Port,
		User:     inputs.MySQL.User,
		Password: inputs.MySQL.Password,
		Database: inputs.MySQL.Database,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("connect to MySQL: %w", err))
		return
	}
	defer mysqlConnector.Close()

	postgresConnector, err := database.ConnectPostgresPreferSSL(r.Context(), config.Database{
		Driver:   config.DatabaseDriverPostgres,
		Host:     inputs.PostgreSQL.Host,
		Port:     request.Postgres.Port,
		User:     inputs.PostgreSQL.User,
		Schema:   inputs.PostgreSQL.Schema,
		Password: inputs.PostgreSQL.Password,
		Database: inputs.PostgreSQL.Database,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("connect to PostgreSQL: %w", err))
		return
	}
	defer postgresConnector.Close()

	options := []migrator.Option{
		migrator.WithTableFilter(request.IncludeTables, request.ExcludeTables),
	}
	if request.LoadMethod != "" {
		options = append(options, migrator.WithLoadMethod(migrator.LoadMethod(request.LoadMethod)))
	}
	engine, err := migrator.New(mysqlConnector, postgresConnector, nil, "dry-run", options...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	report, err := engine.DryRun(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	record, err := s.storage.GetSessionByID(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if record == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("session %q was not found", sessionID))
		return
	}

	tables, err := s.storage.ListMigrationsBySession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	status := "idle"
	errorMessage := ""
	if activeRun := s.runs.get(sessionID); activeRun != nil {
		runState, runErr := activeRun.snapshot()
		status = string(runState)
		if runErr != nil {
			errorMessage = runErr.Error()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         sessionID,
		"status":     status,
		"error":      errorMessage,
		"created_at": record.CreatedAt,
		"tables":     tables,
	})
}

type sseEvent struct {
	ID            int64               `json:"id"`
	Time          time.Time           `json:"time"`
	Level         migrator.EventLevel `json:"level"`
	Message       string              `json:"message"`
	Table         string              `json:"table,omitempty"`
	ProcessedRows int64               `json:"processed_rows,omitempty"`
	TotalRows     int64               `json:"total_rows,omitempty"`
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	activeRun := s.runs.get(sessionID)
	if activeRun != nil {
		if status, _ := activeRun.snapshot(); status != runStatusRunning {
			// The in-process run finished; the store has its full history
			// plus anything an external resume (e.g. the CLI) writes next.
			activeRun = nil
		}
	}
	if activeRun == nil {
		s.streamExternalSession(w, r, sessionID)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming is not supported"))
		return
	}

	var fromID int64
	if lastEventID := r.Header.Get("Last-Event-ID"); lastEventID != "" {
		if parsed, err := strconv.ParseInt(lastEventID, 10, 64); err == nil && parsed >= 0 {
			fromID = parsed + 1
		}
	}

	replay, live, cancel := activeRun.subscribe(fromID)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeEnvelope := func(envelope eventEnvelope) bool {
		payload, err := json.Marshal(sseEvent{
			ID:            envelope.ID,
			Time:          envelope.Event.Time,
			Level:         envelope.Event.Level,
			Message:       envelope.Event.Message,
			Table:         envelope.Event.Table,
			ProcessedRows: envelope.Event.ProcessedRows,
			TotalRows:     envelope.Event.TotalRows,
		})
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", envelope.ID, payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for _, envelope := range replay {
		if !writeEnvelope(envelope) {
			return
		}
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case envelope, open := <-live:
			if !open {
				// Disconnected for lagging; the client reconnects with
				// Last-Event-ID and replays what it missed.
				return
			}
			if !writeEnvelope(envelope) {
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
