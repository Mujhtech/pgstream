package storage

import (
	"time"

	"github.com/guregu/null"
)

type MigrationRecord struct {
	Id         string `json:"id"`
	SessionId  string `json:"session_id"`
	TableName  string `json:"table_name"`
	Status     string `json:"status"`
	LastOffset int64  `json:"last_offset"`
	LastCursor string `json:"last_cursor"`
	// BatchSize records the batch size the writing invocation used, so a
	// resume can size its idempotent replay window correctly.
	BatchSize    int64  `json:"batch_size"`
	RowCount     int64  `json:"row_count"`
	ErrorMessage string `json:"error_message"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    null.Time
}

// SessionEvent is one persisted engine log line or progress update. Events
// are written by the process driving the migration and read by any other
// process (server, status command) that wants to display the log.
type SessionEvent struct {
	Id            int64     `json:"id"`
	SessionId     string    `json:"session_id"`
	Level         string    `json:"level"`
	Message       string    `json:"message"`
	TableName     string    `json:"table_name"`
	ProcessedRows int64     `json:"processed_rows"`
	TotalRows     int64     `json:"total_rows"`
	CreatedAt     time.Time `json:"created_at"`
}

type Session struct {
	Id        string    `json:"id"`
	Connector string    `json:"connector"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	DeletedAt null.Time `json:"deleted_at"`
}
