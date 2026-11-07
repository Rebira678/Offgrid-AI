// Package doctor answers one question before a node goes into the field: is
// this machine in a state where Offgrid will behave? It checks the things that
// went wrong on real hardware in every offline-first system before this one:
// a clock that is wrong after a cold boot, a full or failing disk, a port that
// is taken, a database that is damaged.
//
// It never changes anything. The database is opened read-only (unless Deep is
// asked for, which runs the same checks as `offgrid store verify`).
package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"offgrid/internal/hlc"
	"offgrid/internal/store/sqlstore"
)

// Level is how bad a finding is.
type Level int

const (
	Pass Level = iota // nothing to do
	Warn              // works, but a risk the owner should know about
	Fail              // Offgrid will misbehave here; fix before use
)

func (l Level) String() string { return [...]string{"PASS", "WARN", "FAIL"}[l] }

// Check is one finding.
type Check struct {
	Name   string `json:"name"`
	Level  Level  `json:"-"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func mk(name string, l Level, detail, fix string) Check {
	return Check{Name: name, Level: l, Status: l.String(), Detail: detail, Fix: fix}
}

// Options says what to check. Every field after Deep is a seam for tests; the
// zero value means "ask the real machine".
type Options struct {
	DBPath  string   // the database to inspect; empty skips the database checks
	Listen  []string // addresses Offgrid will want to bind, e.g. 127.0.0.1:7878
	MinFree uint64   // FAIL under this many bytes free next to the database (default 50 MiB); WARN under ten times it
	Deep    bool     // also recompute the hash chain and the view (opens the database read-write, like store verify)
	Version string   // the offgrid version to print

	Now        func() time.Time
	ExePath    func() (string, error)
	FreeBytes  func(dir string) (uint64, error)
	TryListen  func(addr string) error
	NTP        func(ctx context.Context) (synced, known bool)
	RTCPresent func() (present, known bool)
	BlockNames func() []string
}

const defaultMinFree = 50 << 20

func (o *Options) fill() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ExePath == nil {
		o.ExePath = os.Executable
	}
	if o.FreeBytes == nil {
		o.FreeBytes = freeBytes
	}
	if o.TryListen == nil {
		o.TryListen = tryListen
	}
	if o.NTP == nil {
		o.NTP = ntpSynced
	}
	if o.RTCPresent == nil {
		o.RTCPresent = rtcPresent
	}
	if o.BlockNames == nil {
		o.BlockNames = blockNames
	}
	if o.MinFree == 0 {
		o.MinFree = defaultMinFree
	}
}

// Run performs every check. It always returns all of them, in a fixed order, so
// output can be compared between machines.
func Run(ctx context.Context, o Options) []Check {
	o.fill()
	var out []Check
	out = append(out, versionCheck(ctx, o))

	var newest time.Time
	var haveNewest bool
	var db *sql.DB
	if o.DBPath != "" {
		var err error
		db, err = sqlstore.OpenReadOnly(o.DBPath)
		if err != nil {
			out = append(out, dbOpenFailure(o.DBPath, err))
		} else {
			defer db.Close()
			newest, haveNewest = newestOp(ctx, db)
		}
	}
	out = append(out, clockChecks(ctx, o, newest, haveNewest)...)
	out = append(out, diskChecks(o)...)
	out = append(out, portChecks(o)...)
	if db != nil {
		out = append(out, dbChecks(ctx, o, db)...)
	}
	return out
}

// Summarize counts findings by level.
func Summarize(cs []Check) (pass, warn, fail int) {
	for _, c := range cs {
		switch c.Level {
		case Pass:
			pass++
		case Warn:
			warn++
		default:
			fail++
		}
	}
	return
}

// Print writes the findings for a person.
func Print(w io.Writer, cs []Check) {
	fmt.Fprintln(w, "offgrid doctor (changes nothing)")
	for _, c := range cs {
		fmt.Fprintf(w, "  %s  %-9s %s\n", c.Level, c.Name, c.Detail)
		if c.Fix != "" && c.Level != Pass {
			fmt.Fprintf(w, "        fix: %s\n", c.Fix)
		}
	}
	p, wn, f := Summarize(cs)
	fmt.Fprintf(w, "\n%d passed, %d warnings, %d failed\n", p, wn, f)
}

// ---- version ----------------------------------------------------------------

func versionCheck(ctx context.Context, o Options) Check {
	detail := fmt.Sprintf("offgrid %s, %s %s/%s", orUnknown(o.Version), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if v, err := sqliteVersion(ctx); err == nil {
		detail += ", sqlite " + v
	} else {
		return mk("version", Fail, detail+"; the SQLite driver does not work: "+err.Error(), "reinstall a release build; a source build needs a C compiler (build-essential)")
	}
	if runtime.GOOS != "linux" {
		return mk("version", Warn, detail, "only Linux is supported until it has been tested elsewhere")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return mk("version", Warn, detail, "only amd64 and arm64 are built and tested")
	}
	return mk("version", Pass, detail, "")
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func sqliteVersion(ctx context.Context) (string, error) {
	db, err := sqlstore.OpenDB(":memory:")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var v string
	err = db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&v)
	return v, err
}

// ---- clock ------------------------------------------------------------------

// A clock that is wrong is the most likely thing to hurt a site node: a Pi has no
// battery clock, an offline site has no time server, and every new edit is stamped
// from this clock. Wrong by hours, edits sort wrongly; ahead of the log by more
// than the drift limit, other devices refuse the node's new edits.
func clockChecks(ctx context.Context, o Options, newest time.Time, haveNewest bool) []Check {
	now := o.Now()
	var out []Check

	// 1. The binary cannot be newer than the clock: a copied or installed file keeps its
	// build time, so a clock before it is wrong. It needs no network and no constant to rot.
	if exe, err := o.ExePath(); err == nil {
		if fi, err := os.Stat(exe); err == nil && now.Before(fi.ModTime().Add(-time.Minute)) {
			out = append(out, mk("clock", Fail,
				fmt.Sprintf("the clock says %s, but this program was installed on %s", now.UTC().Format(time.RFC3339), fi.ModTime().UTC().Format(time.RFC3339)),
				"set the time (sudo date -s ...) or give the node a battery-backed clock or a LAN time source"))
		}
	}

	// 2. The log's newest op against the clock.
	if haveNewest {
		ahead := newest.Sub(now)
		switch {
		case ahead > hlc.DefaultMaxDrift:
			out = append(out, mk("clock", Fail,
				fmt.Sprintf("the newest edit in the log is %s in the future of this clock. New edits will be stamped even later, and other devices refuse edits more than %s ahead of their own clock", roundDur(ahead), hlc.DefaultMaxDrift),
				"correct the clock first (it is probably behind, e.g. after a power cut with no battery clock). Do not edit until it is right"))
		case ahead > time.Minute:
			out = append(out, mk("clock", Warn,
				fmt.Sprintf("the newest edit in the log is %s ahead of this clock (the clock may have gone backwards)", roundDur(ahead)),
				"check the time; sync is safe but edits made now sort earlier than they should"))
		}
	}

	// 3. Is something keeping it right?
	if synced, known := o.NTP(ctx); known && !synced {
		out = append(out, mk("clock", Warn, "the system clock is not synchronized to a time server",
			"expected on an offline site, but then use a battery-backed clock or one node that serves time to the others"))
	}
	if present, known := o.RTCPresent(); known && !present {
		out = append(out, mk("clock", Warn, "this machine has no hardware clock: after a power cut the time is only what the system last saved",
			"add a battery-backed clock module (Raspberry Pi) or accept that the first edits after a cold boot may carry an old time"))
	}

	if len(out) == 0 {
		detail := now.UTC().Format(time.RFC3339)
		if haveNewest {
			detail += ", not behind the newest edit in the log"
		}
		out = append(out, mk("clock", Pass, detail, ""))
	}
	return out
}

func roundDur(d time.Duration) time.Duration {
	if d > time.Minute {
		return d.Round(time.Second)
	}
	return d
}

func newestOp(ctx context.Context, db *sql.DB) (time.Time, bool) {
	var wall int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts), 0) FROM ops`).Scan(&wall); err != nil || wall == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, wall), true
}

// ntpSynced asks systemd. known is false where there is no timedatectl.
func ntpSynced(ctx context.Context) (synced, known bool) {
	path, err := exec.LookPath("timedatectl")
	if err != nil {
		return false, false
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "show", "-p", "NTPSynchronized", "--value").Output()
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(out)) {
	case "yes":
		return true, true
	case "no":
		return false, true
	}
	return false, false
}

// rtcPresent looks for a hardware clock device. It cannot tell whether it has a battery.
func rtcPresent() (present, known bool) {
	if runtime.GOOS != "linux" {
		return false, false
	}
	ents, err := os.ReadDir("/sys/class/rtc")
	if err != nil {
		return false, false
	}
	return len(ents) > 0, true
}

// ---- disk -------------------------------------------------------------------

func diskChecks(o Options) []Check {
	dir := "."
	if o.DBPath != "" {
		dir = filepath.Dir(o.DBPath)
	}
	var out []Check
	free, err := o.FreeBytes(dir)
	switch {
	case err != nil:
		out = append(out, mk("disk", Warn, "could not read free space for "+dir+": "+err.Error(), ""))
	case free < o.MinFree:
		out = append(out, mk("disk", Fail, fmt.Sprintf("only %s free in %s", human(free), dir),
			"free space, or move the database to a bigger disk: a full disk fails writes in the middle of a commit"))
	case free < 10*o.MinFree:
		out = append(out, mk("disk", Warn, fmt.Sprintf("%s free in %s (getting low)", human(free), dir), "free space before it fills"))
	default:
		out = append(out, mk("disk", Pass, fmt.Sprintf("%s free in %s", human(free), dir), ""))
	}
	if ds := blockKind(o.BlockNames()); ds == "sdcard" {
		out = append(out, mk("storage", Warn, "this machine's only storage is an SD or eMMC card, which can corrupt on power loss and wears out under constant writes",
			"use an SSD or a high-endurance card, and keep a backup"))
	}
	return out
}

// blockKind says "sdcard" when the only block devices are mmcblk*, i.e. nothing
// to boot or store on except an SD or eMMC card. Anything else says "".
func blockKind(names []string) string {
	mmc, other := false, false
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, "mmcblk") && !strings.Contains(n, "boot"):
			mmc = true
		case strings.HasPrefix(n, "loop"), strings.HasPrefix(n, "ram"), strings.HasPrefix(n, "zram"), strings.HasPrefix(n, "dm-"), strings.HasPrefix(n, "mmcblk"):
		default:
			other = true
		}
	}
	if mmc && !other {
		return "sdcard"
	}
	return ""
}

func blockNames() []string {
	ents, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func human(b uint64) string {
	const mib = 1 << 20
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	default:
		return fmt.Sprintf("%.0f MiB", float64(b)/mib)
	}
}

// ---- ports ------------------------------------------------------------------

func portChecks(o Options) []Check {
	var out []Check
	for _, addr := range o.Listen {
		if err := o.TryListen(addr); err != nil {
			out = append(out, mk("port", Warn, fmt.Sprintf("%s cannot be used: %v", addr, err),
				"another program (maybe offgrid itself, already running) holds it; pick another address with -addr or -listen"))
			continue
		}
		out = append(out, mk("port", Pass, addr+" is free", ""))
	}
	return out
}

func tryListen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// ---- database ---------------------------------------------------------------

func dbOpenFailure(path string, err error) Check {
	if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") {
		return mk("database", Fail, "no such database: "+path, "check the path, or start one with: offgrid exec -db FILE ...")
	}
	return mk("database", Fail, "cannot open "+path+" read-only: "+err.Error(),
		"if the node lost power, run: offgrid store -db FILE info (it recovers the write-ahead log), then doctor again")
}

func dbChecks(ctx context.Context, o Options, db *sql.DB) []Check {
	var out []Check

	// integrity_check reads every page: it finds a torn or corrupt file that opens fine.
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return append(out, mk("database", Fail, "integrity check could not run: "+err.Error(), "restore from a backup"))
	}
	var msgs []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			rows.Close()
			return append(out, mk("database", Fail, "integrity check failed to read: "+err.Error(), "restore from a backup"))
		}
		if m != "ok" {
			msgs = append(msgs, m)
		}
	}
	rows.Close()
	if len(msgs) > 0 {
		if len(msgs) > 3 {
			msgs = append(msgs[:3], fmt.Sprintf("(%d more)", len(msgs)-3))
		}
		return append(out, mk("database", Fail, "SQLite reports damage: "+strings.Join(msgs, "; "),
			"do not keep writing to it. Copy it aside as evidence and restore from a backup or a synced peer"))
	}

	var ops int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops`).Scan(&ops)
	detail := fmt.Sprintf("SQLite integrity check passed (%d ops)", ops)
	if fi, err := os.Stat(o.DBPath + "-wal"); err == nil && fi.Size() > 0 {
		detail += "; a write-ahead log is present (normal while running, or after power was cut: it is replayed on the next open)"
	}
	out = append(out, mk("database", Pass, detail, ""))

	if o.Deep {
		out = append(out, deepChecks(ctx, o)...)
	} else {
		out = append(out, mk("chain", Pass, "not checked (add -deep to recompute the hash chain and the view)", ""))
	}
	return out
}

func deepChecks(ctx context.Context, o Options) []Check {
	st, err := sqlstore.Open(ctx, o.DBPath, sqlstore.Options{MustExist: true})
	if err != nil {
		return []Check{mk("chain", Fail, "cannot open for the deep check: "+err.Error(), "")}
	}
	defer st.Close()
	var out []Check
	if rep, err := st.VerifyChain(ctx); err != nil {
		out = append(out, mk("chain", Fail, err.Error(), "the log is evidence: compare with another replica (offgrid sync summary) before changing anything"))
	} else {
		out = append(out, mk("chain", Pass, fmt.Sprintf("hash chain intact over %d ops", rep.Ops), ""))
	}
	if rep, err := st.Verify(ctx); err != nil {
		out = append(out, mk("view", Fail, err.Error(), "offgrid store -db FILE rebuild recomputes the view from the log"))
	} else {
		out = append(out, mk("view", Pass, fmt.Sprintf("the task view matches the log (%d ops, hash %.12s)", rep.Ops, rep.LiveHash), ""))
	}
	return out
}
