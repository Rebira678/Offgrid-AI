package sqlstore

import (
	"strings"

	// This is the ONLY file that names the SQLite driver. mattn/go-sqlite3
	// wraps the C SQLite library, so building needs a C compiler (on Zorin or
	// Ubuntu: sudo apt install build-essential). To use a pure-Go driver
	// instead, change this import, driverName and the DSN options below;
	// nothing else in the project mentions the driver.
	_ "github.com/mattn/go-sqlite3"
)

const driverName = "sqlite3"

// dsn builds the connection string.
//
//	_journal_mode=WAL     readers do not block the writer, and a crash cannot
//	                      leave a half-written database
//	_synchronous=FULL     a commit is on disk before it is acknowledged: an op the
//	                      worker was told "saved" survives a power cut
//	_busy_timeout=5000    wait up to 5 s for a lock instead of failing at once
//	_txlock=immediate     every transaction takes the write lock at its start, so a
//	                      read-then-write transaction cannot deadlock halfway
//	_foreign_keys=on      enforce foreign keys if a later migration adds any
func dsn(path string) string {
	const opts = "_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000&_txlock=immediate&_foreign_keys=on"
	if path == ":memory:" {
		return "file::memory:?" + opts
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	return "file:" + esc + "?" + opts
}
