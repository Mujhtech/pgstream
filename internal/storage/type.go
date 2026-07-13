package storage

import (
	"time"

	"github.com/guregu/null"
)

type MigrationRecord struct {
	Id           string `json:"id"`
	SessionId    string `json:"session_id"`
	TableName    string `json:"table_name"`
	Status       string `json:"status"`
	LastOffset   int64  `json:"last_offset"`
	LastCursor   string `json:"last_cursor"`
	RowCount     int64  `json:"row_count"`
	ErrorMessage string `json:"error_message"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    null.Time
}

type Session struct {
	Id        string    `json:"id"`
	Connector string    `json:"connector"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	DeletedAt null.Time `json:"deleted_at"`
}
