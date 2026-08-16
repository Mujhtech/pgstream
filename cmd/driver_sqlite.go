package main

import (
	// modernc.org/sqlite is a pure-Go driver, so released binaries built
	// with CGO_ENABLED=0 keep the default SQLite metadata storage working.
	// It registers as "sqlite"; database.Connect maps the configured
	// "sqlite3" driver name onto it.
	_ "modernc.org/sqlite"
)
