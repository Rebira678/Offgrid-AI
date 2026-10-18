package sqlstore

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// OpenDB opens a SQLite database with the project's settings (WAL, durable
// commits, busy timeout, one connection) and returns the plain *sql.DB.
//
// It exists so that other packages (the job queue) can keep their own tables
// without ever naming the SQLite driver: this package is the only place that
// knows which driver is in use. The caller owns the result and must Close it.
func OpenDB(path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("sqlstore: empty database path")
	}
	db, err := sql.Open(driverName, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("sqlstore: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // see Open: one writer, and ":memory:" needs one connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlstore: %w", err)
	}
	return db, nil
}

// OpenReadOnly opens an existing SQLite database for reading only. It never
// creates the file, never changes its schema, and cannot write: it is for
// viewers (the dashboard) that must not disturb the process doing the work.
// A missing file is an error, not an empty database.
func OpenReadOnly(path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("sqlstore: empty database path")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("sqlstore: %w", err)
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	db, err := sql.Open(driverName, "file:"+esc+"?mode=ro&_busy_timeout=2000&_query_only=1")
	if err != nil {
		return nil, fmt.Errorf("sqlstore: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlstore: %w", err)
	}
	return db, nil
}
