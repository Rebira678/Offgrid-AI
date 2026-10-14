package sqlstore

import (
	"database/sql"
	"errors"
	"fmt"
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
