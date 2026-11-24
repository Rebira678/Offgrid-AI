// Package sqlstore is Offgrid's persistent task store: SQLite, event-sourced.
//
// THE DESIGN IN ONE PARAGRAPH. Nothing is ever updated in place. Every change
// is appended to the ops table as an immutable fact ("task T3 was created",
// "task T3 became done"), tagged with the replica that recorded it and that
// replica's own counter. The tasks and task_notes tables are a materialized
// VIEW of the log, rebuilt by replaying it, so they can be deleted at any time
// without losing anything. The log is the truth; the tables are a cache.
//
// WHY. Sync (Days 15 to 20) needs to know WHAT HAPPENED, not just what the
// state is. Comparing two states cannot tell "I edited it and you did not" from
// "you deleted it and I edited it". Comparing two logs can: each op has an
// identity (node, seq), so replicas exchange only the ops the other lacks, and
// applying the same op twice is detectable instead of harmful.
package sqlstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"offgrid/internal/blobstore"
	"offgrid/internal/hlc"
	"offgrid/internal/task"
)

// Store implements task.Store on top of SQLite.
type Store struct {
	db   *sql.DB
	node string
	now  func() time.Time

	// clock stamps every op this replica records. It is restored from the
	// newest stored timestamp on open, so a restart can never go backwards.
	clock *hlc.Clock

	// signer signs every op this replica writes (nil: unsigned, as before Day 36).
	signer        Signer
	requireSigned bool

	// rdb serves the plain reads (Get, List) from connections of their own, so a read does not
	// wait behind a commit that is on its way to disk. nil for ":memory:" databases (each
	// connection to one is a separate database) and when the extra connections cannot be opened:
	// reads then share db, as before. See read.
	rdb *sql.DB

	blobDir     string // where attachment bytes live; "" when there is nowhere
	blobMax     int64
	blobReserve int64
	blobMu      sync.Mutex
	blobs       *blobstore.Store // opened on first use

	gc   *committer // the one writer: group commit (groupcommit.go)
	pool *stmtPool  // prepared statements shared by transactions (stmtcache.go)
}

var _ task.Store = (*Store)(nil)

// Options configures Open.
type Options struct {
	// NodeID names this replica. Empty means: use the one stored in the
	// database, or generate a random one the first time. Once a database has a
	// node ID, a different one is refused: the ID is the replica's identity and
	// the sequence numbers only make sense under it.
	NodeID string
	// Now is the physical clock the hybrid logical clock reads. nil means
	// time.Now. Ops are stamped from the HLC, not from Now directly: if Now
	// stalls or goes backwards, op timestamps still increase.
	Now func() time.Time
	// MaxDrift bounds how far ahead of Now a timestamp received from another
	// replica may be (see hlc.Config). Zero is hlc.DefaultMaxDrift.
	MaxDrift time.Duration
	// MustExist makes Open refuse a database file that is not there, with
	// ErrNoDatabase, instead of creating it. Every command that only reads or
	// edits what is already stored sets it: creating a database is a decision
	// (it also fixes the replica's node ID for good), and a mistyped path must
	// not make that decision. The driver is also told not to create the file
	// (mode=rw), so a file deleted between the check and the open cannot be
	// recreated either.
	MustExist bool
	// Signer is this device's key. With one, every op this replica writes is
	// signed (sigs.go), and Open refuses a database whose ops were signed by a
	// different device (ErrKeyChange). nil writes unsigned ops, unless the
	// database has signed before: then writing is refused (ErrUnsigned), so a
	// missing key file cannot quietly turn signing off.
	Signer Signer
	// RequireSigned makes ApplyRemote refuse ops that carry no signature
	// (ErrUnsigned), and makes Open fail without a Signer. For a replica that
	// has no legacy history to receive.
	RequireSigned bool
	// FlushWindow is how long the writer waits for more writes to share a commit after the first
	// one arrives. Zero (the default) waits for nothing: writes that are already queued share a
	// commit, and batches form on their own while a commit is on its way to disk. A window adds
	// that much latency to a lone write and buys bigger batches for bursty writers.
	FlushWindow time.Duration
	// MaxBatch caps how many writes share one commit. Zero is 128.
	MaxBatch int
	// BlobDir is where attachment bytes are kept (see attachments.go). Empty means "<database path>.blobs"
	// for a database file, and no blob storage for ":memory:" (AddAttachment then fails with ErrNoBlobStore).
	// The directory is created when the first attachment is stored, not before.
	BlobDir string
	// MaxBlobBytes caps one attachment. Zero is blobstore.DefaultMaxBytes (25 MiB).
	MaxBlobBytes int64
	// BlobReserveBytes is the disk space an attachment may never eat into: a blob is accepted only if it fits
	// and leaves this much free. Zero is blobstore.DefaultReserve (64 MiB).
	BlobReserveBytes int64
}

// ErrNoDatabase means there is no database at the path and the caller asked
// for an existing one (Options.MustExist). The error text names the path.
var ErrNoDatabase = errors.New("no such database")

// ErrNodeMismatch means the database belongs to a different replica.
var ErrNodeMismatch = errors.New("database belongs to a different node")

var nodeIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Open opens (creating if needed) the database at path. Use ":memory:" for a
// throwaway store. It migrates the schema, settles the node ID, and makes sure
// the materialized view is present, current and caught up with the log.
func Open(ctx context.Context, path string, opt Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlstore: empty database path")
	}
	if opt.NodeID != "" && !nodeIDRe.MatchString(opt.NodeID) {
		return nil, fmt.Errorf("sqlstore: node ID %q must be 1 to 64 letters, digits, '.', '_' or '-'", opt.NodeID)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sqlstore: %w", err)
	}
	if opt.RequireSigned && opt.Signer == nil {
		return nil, errors.New("sqlstore: RequireSigned needs a device key (Signer)")
	}
	if opt.Signer != nil {
		if err := checkSigner(opt.Signer); err != nil {
			return nil, err
		}
	}
	conn := dsn(path)
	var created bool
	if !opt.MustExist && path != ":memory:" {
		if err := checkNonDanglingSymlink(path); err != nil {
			return nil, err
		}
		created = createPrivate(path)
	}
	if opt.MustExist && path != ":memory:" {
		if err := requireFile(path); err != nil {
			return nil, err
		}
		conn += "&mode=rw" // never create: see Options.MustExist
	}
	db, err := sql.Open(driverName, conn)
	if err != nil {
		if created {
			removeEmptyDBStub(path)
		}
		return nil, fmt.Errorf("sqlstore: open %s: %w", path, err)
	}
	// One connection: SQLite has one writer anyway, and it makes ":memory:"
	// databases behave (each connection to ":memory:" is a separate database).
	db.SetMaxOpenConns(1)

	s := &Store{db: db, now: opt.Now, signer: opt.Signer, requireSigned: opt.RequireSigned, blobMax: opt.MaxBlobBytes, blobReserve: opt.BlobReserveBytes}
	switch {
	case opt.BlobDir != "":
		s.blobDir = opt.BlobDir
	case path != ":memory:" && !strings.HasPrefix(path, "file:"):
		s.blobDir = path + ".blobs"
	}
	s.pool = newStmtPool(db)
	s.gc = newCommitter(s, opt.FlushWindow, opt.MaxBatch)
	if s.now == nil {
		s.now = time.Now
	}
	s.clock = hlc.New(hlc.Config{Now: s.now, MaxDrift: opt.MaxDrift})
	if err := s.init(ctx, opt); err != nil {
		s.gc.close()
		s.pool.close()
		db.Close()
		if created && !isBusyOrLocked(err) {
			removeEmptyDBStub(path)
		}
		if opt.MustExist && path != ":memory:" {
			// The file was there a moment ago; if it is gone now, say so plainly.
			if gone := requireFile(path); gone != nil {
				return nil, gone
			}
		}
		return nil, err
	}
	if created {
		ensurePrivate(path)
	}
	s.rdb = openReaders(path)
	return s, nil
}

func ensurePrivate(path string) {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return
	}
	_ = os.Chmod(path, 0o600)
	_ = os.Chmod(path+"-wal", 0o600)
	_ = os.Chmod(path+"-shm", 0o600)
}

// readConns is the size of the reader pool: enough for the UI, the sync server and a status
// command to read at once, few enough that they cannot starve the single writer of CPU.
const readConns = 4

// openReaders opens read-only connections to the same file, or returns nil to mean "use the
// writer's connection". WAL lets them read while the writer commits: a read sees the last
// committed state, which after a write returns includes that write.
func openReaders(path string) *sql.DB {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	db, err := sql.Open(driverName, "file:"+esc+"?mode=ro&_busy_timeout=5000&_query_only=1")
	if err != nil {
		return nil
	}
	db.SetMaxOpenConns(readConns)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil
	}
	return db
}

// read is where plain reads go.
func (s *Store) read() *sql.DB {
	if s.rdb != nil {
		return s.rdb
	}
	return s.db
}

// checkNonDanglingSymlink refuses creating a brand-new database through a
// dangling symlink (which would bypass O_CREATE|O_EXCL 0600 creation and
// let SQLite create the target world-readable).
func checkNonDanglingSymlink(path string) error {
	lfi, err := os.Lstat(path)
	if err != nil || lfi.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sqlstore: refusal: dangling symlink %s", path)
	}
	return nil
}

func isBusyOrLocked(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "locked") || strings.Contains(msg, "busy")
}

func removeEmptyDBStub(path string) {
	if _, err := os.Stat(path + "-wal"); err == nil {
		return
	}
	if _, err := os.Stat(path + "-shm"); err == nil {
		return
	}
	fi, err := os.Stat(path)
	if err == nil && fi.Size() == 0 {
		_ = os.Remove(path)
	}
}

// createPrivate makes the database file, mode 0600, if it does not exist yet.
// SQLite would create it with the process's default mode (0644 under the usual
// umask), and it gives the -wal and -shm files the main file's mode: so on a
// shared or stolen machine every local user could read the tasks. An existing
// file is never touched, and any failure is left for SQLite to report in its
// own words.
func createPrivate(path string) bool {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return false
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_ = f.Close()
		return true
	}
	return false
}

// requireFile returns ErrNoDatabase (naming the path) if path is not an
// existing file. A folder is reported as itself, not as a missing database.
func requireFile(path string) error {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %s", ErrNoDatabase, path)
	case err != nil:
		return fmt.Errorf("sqlstore: %w", err)
	case fi.IsDir():
		return fmt.Errorf("sqlstore: %s is a folder, not a database file", path)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("sqlstore: %s is not a regular file", path)
	}
	return nil
}

func (s *Store) init(ctx context.Context, opt Options) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlstore: %w", err)
	}
	// A database that has no hash chain yet gets one now, from the ops as they
	// stand. Only then: a chain that goes missing later must be reported by
	// Verify, never quietly rebuilt from the ops it was meant to protect.
	var hadChain int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'op_chain'`).Scan(&hadChain); err != nil {
		return fmt.Errorf("sqlstore: %w", err)
	}
	if err := migrate(ctx, s.db, migrations); err != nil {
		return err
	}
	if hadChain == 0 {
		if err := s.backfillChain(ctx); err != nil {
			return err
		}
	}
	node, err := s.settleNodeID(ctx, opt.NodeID)
	if err != nil {
		return err
	}
	s.node = node
	if err := s.checkBinding(ctx); err != nil {
		return err
	}
	if err := s.restoreClock(ctx); err != nil {
		return err
	}
	return s.ensureProjection(ctx)
}

// restoreClock raises the clock to the newest timestamp in the log. Without it
// a restart after the physical clock was reset (a dead battery, a step back by
// NTP) would issue timestamps older than ones already stored, and a later edit
// would sort before an earlier one.
func (s *Store) restoreClock(ctx context.Context) error {
	last, err := s.newestStamp(ctx, s.db)
	if err != nil {
		return err
	}
	s.clock.Restore(last)
	return nil
}

func (s *Store) newestStamp(ctx context.Context, x execer) (hlc.Timestamp, error) {
	var wall int64
	var counter uint32
	err := x.QueryRowContext(ctx, `SELECT ts, counter FROM ops ORDER BY ts DESC, counter DESC LIMIT 1`).Scan(&wall, &counter)
	if errors.Is(err, sql.ErrNoRows) {
		return hlc.Timestamp{}, nil
	}
	if err != nil {
		return hlc.Timestamp{}, err
	}
	return hlc.Timestamp{Wall: wall, Counter: counter}, nil
}

// Clock is this replica's hybrid logical clock. The sync layer calls Update on
// it with the timestamps of the ops it receives, so that whatever this replica
// does next is stamped after everything it has seen.
func (s *Store) Clock() *hlc.Clock { return s.clock }

// settleNodeID returns this database's node ID, creating it if this is a new
// database, and refusing a conflicting request.
func (s *Store) settleNodeID(ctx context.Context, want string) (string, error) {
	have, ok, err := getMeta(ctx, s.db, "node_id")
	if err != nil {
		return "", err
	}
	switch {
	case ok && want != "" && want != have:
		return "", fmt.Errorf("%w: it is %q, but %q was requested", ErrNodeMismatch, have, want)
	case ok:
		return have, nil
	}
	if want == "" {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("sqlstore: generate node ID: %w", err)
		}
		want = "n-" + hex.EncodeToString(b[:])
	}
	if err := setMeta(ctx, s.db, "node_id", want); err != nil {
		return "", err
	}
	return want, nil
}

// ensureProjection makes the materialized view usable: it rebuilds from the log
// if the view is missing or was built by a different projectionVersion, and
// applies any ops the view has not seen yet.
func (s *Store) ensureProjection(ctx context.Context) error {
	version, _, err := getMeta(ctx, s.db, "projection_version")
	if err != nil {
		return err
	}
	var present int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('tasks', 'task_notes', 'answers', 'suggestions', 'attachments')`).Scan(&present); err != nil {
		return err
	}
	if version != projectionVersion || present != 5 {
		_, err := s.Rebuild(ctx)
		return err
	}
	return s.catchUp(ctx)
}

// catchUp applies ops recorded after the last one the view has applied. Normal
// writes keep the view current in the same transaction, so this only does work
// if ops arrived another way (later: from sync).
func (s *Store) catchUp(ctx context.Context) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	applied, err := appliedPos(ctx, tx)
	if err != nil {
		return err
	}
	n, last, err := replay(ctx, tx, tx, applied)
	if err != nil {
		return err
	}
	if n > 0 {
		if err = setMeta(ctx, tx, "applied_pos", strconv.FormatInt(last, 10)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close releases the database.
func (s *Store) Close() error {
	s.gc.close() // finish the batch in flight, refuse the rest, stop the writer
	s.pool.close()
	if s.rdb != nil {
		s.rdb.Close()
	}
	return s.db.Close()
}

// NodeID is this replica's identity.
func (s *Store) NodeID() string { return s.node }

// Signer returns the store's configured device key, or nil if none.
func (s *Store) Signer() Signer { return s.signer }

// ---- meta -------------------------------------------------------------------

func getMeta(ctx context.Context, x execer, key string) (string, bool, error) {
	var v string
	err := x.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func setMeta(ctx context.Context, x execer, key, value string) error {
	_, err := x.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func appliedPos(ctx context.Context, x execer) (int64, error) {
	v, ok, err := getMeta(ctx, x, "applied_pos")
	if err != nil || !ok {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// ---- writing ----------------------------------------------------------------

// write runs fn in a transaction, shared with other writers that are waiting (group commit,
// groupcommit.go). If fn returns an error, nothing it did, in the log or in the view, is
// kept, and other writers' work is not affected. write returns after the commit that holds
// fn's changes has reached the disk.
func (s *Store) write(ctx context.Context, fn func(tx execer) error) error {
	return s.gc.submit(ctx, fn)
}

// record appends one op to the log and applies it to the view, in the caller's
// transaction. The two happen together or not at all, so the view can never be
// ahead of, or behind, the log after a commit.
//
// This is where write-time decisions are frozen into the op: the sequence
// number and the hybrid logical clock timestamp are chosen here, once, and
// stored.
func (s *Store) record(ctx context.Context, tx execer, kind string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sqlstore: encode %s: %w", kind, err)
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM ops WHERE node_id = ?`, s.node).Scan(&seq); err != nil {
		return err
	}
	if err := s.refuseIfRevoked(ctx, tx, kind, seq); err != nil {
		return err
	}
	stamp := s.clock.Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES (?, ?, ?, ?, ?, ?)`,
		s.node, seq, stamp.Wall, stamp.Counter, kind, string(raw))
	if err != nil {
		return fmt.Errorf("sqlstore: append %s: %w", kind, err)
	}
	pos, err := res.LastInsertId()
	if err != nil {
		return err
	}
	op := Op{Pos: pos, NodeID: s.node, Seq: seq, TS: stamp.Time(), Counter: stamp.Counter, Kind: kind, Payload: raw}
	hash, err := chainAppend(ctx, tx, op)
	if err != nil {
		return err
	}
	if err := s.signLocal(ctx, tx, op, hash); err != nil {
		return err
	}
	if err := applyOp(ctx, tx, op); err != nil {
		return err
	}
	return setMeta(ctx, tx, "applied_pos", strconv.FormatInt(pos, 10))
}

func (s *Store) Create(ctx context.Context, n task.NewTask) (task.Task, bool, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, false, err
	}
	title := strings.TrimSpace(n.Title)
	if title == "" {
		return task.Task{}, false, fmt.Errorf("%w: empty title", task.ErrInvalid)
	}
	if _, err := task.ParsePriority(string(n.Priority)); err != nil {
		return task.Task{}, false, fmt.Errorf("%w: %v", task.ErrInvalid, err)
	}

	var out task.Task
	var created bool
	err := s.write(ctx, func(tx execer) error {
		// An unfinished task with the same title and site is the same task:
		// return it and record nothing. The check and the insert share one
		// write-locked transaction, so two racing requests cannot both pass it.
		var dupID string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM tasks WHERE deleted = 0 AND status != 'done' AND title_key = ? AND site_key = ? ORDER BY n LIMIT 1`,
			task.TitleKey(title), strings.ToLower(n.Site)).Scan(&dupID)
		if err == nil {
			out, err = getTask(ctx, tx, dupID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// The ID is decided HERE and written into the op, never at apply time.
		// It is always "T<n>.<replica>". Before Day 24 a replica that had only
		// its own ops used a plain "T<n>", on the theory that nobody else was
		// numbering tasks yet; the simulator (Day 24) showed the flaw: two
		// replicas that each create their first task before they ever meet both
		// pick "T1", and from then on they can never sync. Naming the replica
		// makes IDs unique with nobody agreeing on anything. (Plain IDs already
		// in a database stay valid, and "T5" still finds "T5.laptop": see
		// task.Resolve.)
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops WHERE kind = ?`, KindTaskCreated).Scan(&count); err != nil {
			return err
		}
		id := fmt.Sprintf("T%d.%s", count+1, s.node)
		if err := s.record(ctx, tx, KindTaskCreated, createdPayload{ID: id, Title: title, Site: n.Site, Priority: n.Priority}); err != nil {
			return err
		}
		created = true
		out, err = getTask(ctx, tx, id)
		return err
	})
	if err != nil {
		return task.Task{}, false, err
	}
	return out, created, nil
}

func (s *Store) Update(ctx context.Context, id string, c task.Changes) (task.Task, bool, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, false, err
	}
	if c.Status == nil && c.Priority == nil {
		return task.Task{}, false, fmt.Errorf("%w: nothing to change", task.ErrInvalid)
	}
	if c.Status != nil {
		if _, err := task.ParseStatus(string(*c.Status)); err != nil {
			return task.Task{}, false, fmt.Errorf("%w: %v", task.ErrInvalid, err)
		}
	}
	if c.Priority != nil {
		if _, err := task.ParsePriority(string(*c.Priority)); err != nil {
			return task.Task{}, false, fmt.Errorf("%w: %v", task.ErrInvalid, err)
		}
	}

	var out task.Task
	var changed bool
	err := s.write(ctx, func(tx execer) error {
		cur, err := getTask(ctx, tx, id)
		if err != nil {
			return err
		}
		// Record only what actually changes. An update that changes nothing
		// leaves no trace in the log.
		var p updatedPayload
		p.ID = id
		if c.Status != nil && *c.Status != cur.Status {
			p.Status = c.Status
		}
		if c.Priority != nil && *c.Priority != cur.Priority {
			p.Priority = c.Priority
		}
		if p.Status == nil && p.Priority == nil {
			out = cur
			return nil
		}
		if err := s.record(ctx, tx, KindTaskUpdated, p); err != nil {
			return err
		}
		changed = true
		out, err = getTask(ctx, tx, id)
		return err
	})
	if err != nil {
		return task.Task{}, false, err
	}
	return out, changed, nil
}

func (s *Store) AddNote(ctx context.Context, id, text string) (task.Task, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return task.Task{}, fmt.Errorf("%w: empty note", task.ErrInvalid)
	}
	var out task.Task
	err := s.write(ctx, func(tx execer) error {
		if _, err := getTask(ctx, tx, id); err != nil {
			return err
		}
		if err := s.record(ctx, tx, KindNoteAdded, notePayload{ID: id, Text: text}); err != nil {
			return err
		}
		var err error
		out, err = getTask(ctx, tx, id)
		return err
	})
	return out, err
}

// DeleteTask removes a task from every list. It records a fact ("this replica
// deleted T3, knowing what it knew") rather than erasing anything: the task's
// ops stay in the log. If another replica edits the task without having seen
// this delete, the task comes back when they meet (add-wins).
func (s *Store) DeleteTask(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.write(ctx, func(tx execer) error {
		if _, err := getTask(ctx, tx, id); err != nil {
			return err
		}
		seen, err := summaryOf(ctx, tx)
		if err != nil {
			return err
		}
		return s.record(ctx, tx, KindTaskRemoved, removedPayload{ID: id, Seen: seen})
	})
}

// RemoveNote removes one note of a task, named by its position (1 is the
// first note currently shown). Only that note goes: a note someone adds at the
// same time, on another replica, is a different note and stays.
func (s *Store) RemoveNote(ctx context.Context, id string, number int) (task.Task, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, err
	}
	var out task.Task
	err := s.write(ctx, func(tx execer) error {
		if _, err := getTask(ctx, tx, id); err != nil {
			return err
		}
		if number < 1 {
			return fmt.Errorf("%w: notes are numbered from 1", task.ErrInvalid)
		}
		var node string
		var seq int64
		err := tx.QueryRowContext(ctx,
			`SELECT node, seq FROM task_notes WHERE task_id = ? AND removed = 0 ORDER BY n LIMIT 1 OFFSET ?`, id, number-1).Scan(&node, &seq)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s has no note %d", task.ErrNotFound, id, number)
		}
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, KindNoteRemoved, noteRemovedPayload{ID: id, Node: node, Seq: seq}); err != nil {
			return err
		}
		out, err = getTask(ctx, tx, id)
		return err
	})
	return out, err
}

var _ task.Remover = (*Store)(nil)

// ---- reading ----------------------------------------------------------------

const taskColumns = `id, title, site, priority, status, created_at, updated_at, done_at`

func scanTask(row interface{ Scan(...any) error }) (task.Task, error) {
	var t task.Task
	var prio, status string
	var created, updated, done int64
	if err := row.Scan(&t.ID, &t.Title, &t.Site, &prio, &status, &created, &updated, &done); err != nil {
		return task.Task{}, err
	}
	t.Priority, t.Status = task.Priority(prio), task.Status(status)
	t.CreatedAt, t.UpdatedAt, t.DoneAt = fromNanos(created), fromNanos(updated), fromNanos(done)
	return t, nil
}

// getTask loads one task with its notes, or task.ErrNotFound.
func getTask(ctx context.Context, x execer, id string) (task.Task, error) {
	t, err := scanTask(x.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ? AND deleted = 0`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, task.ErrNotFound
	}
	if err != nil {
		return task.Task{}, err
	}
	rows, err := x.QueryContext(ctx, `SELECT text, at FROM task_notes WHERE task_id = ? AND removed = 0 ORDER BY n`, id)
	if err != nil {
		return task.Task{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var n task.Note
		var at int64
		if err := rows.Scan(&n.Text, &at); err != nil {
			return task.Task{}, err
		}
		n.At = fromNanos(at)
		t.Notes = append(t.Notes, n)
	}
	return t, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (task.Task, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, err
	}
	return getTask(ctx, s.read(), id)
}

func (s *Store) List(ctx context.Context, f task.Filter) ([]task.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return listTasks(ctx, s.read(), f)
}

// listTasks is shared with the state hash, which reads the whole view.
func listTasks(ctx context.Context, x execer, f task.Filter) ([]task.Task, error) {
	where := []string{"deleted = 0"}
	var args []any
	switch {
	case f.Status != "":
		where, args = append(where, "status = ?"), append(args, string(f.Status))
	case f.ExcludeDone:
		where = append(where, "status != 'done'")
	}
	if f.Priority != "" {
		where, args = append(where, "priority = ?"), append(args, string(f.Priority))
	}
	if f.Site != "" {
		where, args = append(where, "site_key = ?"), append(args, strings.ToLower(f.Site))
	}
	if f.Within != nil {
		where, args = append(where, "created_at >= ? AND created_at < ?"), append(args, nanos(f.Within.Start), nanos(f.Within.End))
	}
	q := `SELECT ` + taskColumns + ` FROM tasks`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY n`

	rows, err := x.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []task.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Notes for the tasks found, in one pass.
	if len(out) == 0 {
		return out, nil
	}
	index := make(map[string]int, len(out))
	for i, t := range out {
		index[t.ID] = i
	}
	nrows, err := x.QueryContext(ctx, `SELECT task_id, text, at FROM task_notes WHERE removed = 0 ORDER BY task_id, n`)
	if err != nil {
		return nil, err
	}
	defer nrows.Close()
	for nrows.Next() {
		var id string
		var n task.Note
		var at int64
		if err := nrows.Scan(&id, &n.Text, &at); err != nil {
			return nil, err
		}
		n.At = fromNanos(at)
		if i, ok := index[id]; ok {
			out[i].Notes = append(out[i].Notes, n)
		}
	}
	return out, nrows.Err()
}
