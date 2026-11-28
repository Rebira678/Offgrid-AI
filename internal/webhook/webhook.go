// Package webhook tells other programs when something changes (v0: not yet proven by use, see
// docs/api/versioning.md). It reads the op log, turns the ops that matter into events, and POSTs each one,
// signed, to the URLs the machine's owner registered with the command line.
//
// Hooks are configured only from the command line, never through the API: a URL the node will POST to is the
// owner's decision, and a paired phone must not be able to point the node at another machine on the network.
//
// Delivery is at least once and best effort: a receiver that is down for longer than the retries (about 20
// seconds) misses the event, and a receiver can see the same event twice (the ID is stable: dedupe on it). The
// API is the source of truth; an event is a nudge to go and read it. By default an event carries IDs and the
// changed values, not text; a hook must ask for titles and notes with include_text.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"offgrid/internal/store/sqlstore"
)

// Event types.
const (
	EventPing               = "ping"
	EventTaskCreated        = "task.created"
	EventTaskUpdated        = "task.updated"
	EventTaskNoteAdded      = "task.note_added"
	EventTaskRemoved        = "task.removed"
	EventSuggestionMade     = "suggestion.made"
	EventSuggestionResolved = "suggestion.resolved"
	EventAttachmentAdded    = "attachment.added"
)

// Types lists every event a hook may subscribe to.
var Types = []string{EventTaskCreated, EventTaskUpdated, EventTaskNoteAdded, EventTaskRemoved, EventSuggestionMade, EventSuggestionResolved, EventAttachmentAdded}

// Event is the JSON a receiver gets.
type Event struct {
	ID   string         `json:"id"` // "<node>:<seq>" of the op: the same on every delivery of this event
	Type string         `json:"type"`
	At   time.Time      `json:"at"`
	Node string         `json:"node"` // the replica that made the change
	Task string         `json:"task,omitempty"`
	Data map[string]any `json:"data,omitempty"`
}

// FromOp turns one op into an event. ok is false for ops that are not events (membership, answers, history).
// Text (titles, notes, file names) is left out unless includeText.
func FromOp(op sqlstore.Op, includeText bool) (ev Event, ok bool) {
	ev = Event{ID: op.NodeID + ":" + strconv.FormatInt(op.Seq, 10), At: op.TS.UTC(), Node: op.NodeID, Data: map[string]any{}}
	var p struct {
		ID         string `json:"id"`
		Task       string `json:"task"`
		Title      string `json:"title"`
		Site       string `json:"site"`
		Text       string `json:"text"`
		Status     string `json:"status"`
		Priority   string `json:"priority"`
		By         string `json:"by"`
		Resolution string `json:"resolution"`
		Hash       string `json:"hash"`
		Type       string `json:"type"`
		Name       string `json:"name"`
		Size       int64  `json:"size"`
		SugNode    string `json:"node"`
		SugSeq     int64  `json:"seq"`
	}
	if json.Unmarshal(op.Payload, &p) != nil {
		return Event{}, false
	}
	set := func(k, v string) {
		if v != "" {
			ev.Data[k] = v
		}
	}
	switch op.Kind {
	case sqlstore.KindTaskCreated:
		ev.Type, ev.Task = EventTaskCreated, p.ID
		set("priority", p.Priority)
		if includeText {
			set("title", p.Title)
			set("site", p.Site)
		}
	case sqlstore.KindTaskUpdated:
		ev.Type, ev.Task = EventTaskUpdated, p.ID
		set("status", p.Status)
		set("priority", p.Priority)
	case sqlstore.KindNoteAdded:
		ev.Type, ev.Task = EventTaskNoteAdded, p.ID
		if includeText {
			set("text", p.Text)
		}
	case sqlstore.KindTaskRemoved:
		ev.Type, ev.Task = EventTaskRemoved, p.ID
	case sqlstore.KindSuggestionMade:
		ev.Type, ev.Task = EventSuggestionMade, p.Task
		set("by", p.By)
		set("status", p.Status)
		set("priority", p.Priority)
	case sqlstore.KindSuggestionResolved:
		ev.Type = EventSuggestionResolved
		set("resolution", p.Resolution)
		set("suggestion", p.SugNode+"."+strconv.FormatInt(p.SugSeq, 10))
	case sqlstore.KindAttachmentAdded:
		ev.Type, ev.Task = EventAttachmentAdded, p.Task
		set("hash", p.Hash)
		set("type", p.Type)
		if p.Size > 0 {
			ev.Data["size"] = p.Size
		}
		if includeText {
			set("name", p.Name)
		}
	default:
		return Event{}, false
	}
	if len(ev.Data) == 0 {
		ev.Data = nil
	}
	return ev, true
}

// ---- signing

// Headers a receiver reads.
const (
	HeaderSignature = "X-Offgrid-Signature" // "v1=" + hex HMAC-SHA256 of timestamp + "." + body
	HeaderTimestamp = "X-Offgrid-Timestamp" // unix seconds
	HeaderEvent     = "X-Offgrid-Event"     // the event type
	HeaderDelivery  = "X-Offgrid-Delivery"  // the event ID
)

// Tolerance is how far a timestamp may be from the receiver's clock before Verify refuses it (replay guard).
const Tolerance = 5 * time.Minute

// Sign returns the signature for a body sent at ts.
func Sign(secret string, ts time.Time, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(ts.Unix(), 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "v1=" + hex.EncodeToString(m.Sum(nil))
}

// Verify is what a receiver runs: it checks the signature in constant time and that the timestamp is recent.
func Verify(secret, signature, timestamp string, body []byte, now time.Time) error {
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("the timestamp is not a number")
	}
	ts := time.Unix(sec, 0)
	if d := now.Sub(ts); d > Tolerance || d < -Tolerance {
		return errors.New("the timestamp is too far from now")
	}
	if !hmac.Equal([]byte(signature), []byte(Sign(secret, ts, body))) {
		return errors.New("the signature does not match")
	}
	return nil
}

// ---- the hooks file

// Hook is one registered receiver.
type Hook struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Secret      string    `json:"secret"`
	Events      []string  `json:"events,omitempty"` // empty: every type
	IncludeText bool      `json:"include_text,omitempty"`
	From        int64     `json:"from"` // log position when it was added: it never sees older changes
	Created     time.Time `json:"created"`
}

// File is the hooks file: the hooks and how far down the log the dispatcher has read.
type File struct {
	Hooks  []Hook `json:"hooks"`
	Cursor int64  `json:"cursor"`
}

// Load reads the file; a missing file is an empty one.
func Load(path string) (File, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return File{}, fmt.Errorf("%s is not a hooks file: %v", path, err)
	}
	return f, nil
}

// Save writes the file atomically with mode 0600: it holds the secrets.
func Save(path string, f File) error {
	b, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hooks-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ValidateURL refuses what a hook must not point at: anything but http or https, a URL with a password in it
// (it would be stored and printed), or no host.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("the URL must start with http:// or https://")
	}
	if u.Hostname() == "" {
		return errors.New("the URL has no host")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("the URL port must be between 1 and 65535")
		}
	}
	if u.User != nil {
		return errors.New("the URL must not contain a user name or password; put credentials in the receiver, and use the signature")
	}
	if u.Fragment != "" {
		return errors.New("the URL must not have a #fragment")
	}
	return nil
}

// NewHook makes a hook with a fresh secret. from is the log position now.
func NewHook(rawURL string, events []string, includeText bool, from int64, now time.Time) (Hook, error) {
	if err := ValidateURL(rawURL); err != nil {
		return Hook{}, err
	}
	known := map[string]bool{"*": true}
	for _, t := range Types {
		known[t] = true
	}
	var evs []string
	for _, e := range events {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !known[e] {
			return Hook{}, fmt.Errorf("%q is not an event type; use one of %s, or *", e, strings.Join(Types, ", "))
		}
		if e == "*" {
			evs = nil
			break
		}
		evs = append(evs, e)
	}
	b := make([]byte, 32+4)
	if _, err := rand.Read(b); err != nil {
		return Hook{}, err
	}
	return Hook{ID: "wh_" + hex.EncodeToString(b[32:]), URL: rawURL, Secret: hex.EncodeToString(b[:32]), Events: evs,
		IncludeText: includeText, From: from, Created: now.UTC()}, nil
}

func (h Hook) wants(typ string) bool {
	if len(h.Events) == 0 {
		return true
	}
	for _, e := range h.Events {
		if e == typ {
			return true
		}
	}
	return false
}

// ---- delivery

// Source is the part of a store the dispatcher reads.
type Source interface {
	Ops(ctx context.Context, after int64, limit int) ([]sqlstore.Op, error)
}

// Dispatcher reads the log and delivers events.
type Dispatcher struct {
	Source    Source
	Path      string // the hooks file
	Client    *http.Client
	Now       func() time.Time
	Poll      time.Duration // how often to look for new ops (default 2s)
	Backoff   []time.Duration
	Sleep     func(ctx context.Context, d time.Duration) // default: a timer that obeys ctx
	Logf      func(format string, args ...any)
	Delivered atomic.Int64
	Failed    atomic.Int64
}

// DefaultBackoff: three attempts, waiting 1 s and then 4 s between them, so a dead receiver costs about
// 15 seconds per event (three 5-second timeouts plus the waits) and never blocks the log reader for longer.
var DefaultBackoff = []time.Duration{time.Second, 4 * time.Second}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dispatcher) logf(f string, a ...any) {
	if d.Logf != nil {
		d.Logf(f, a...)
	}
}

func (d *Dispatcher) sleep(ctx context.Context, dur time.Duration) {
	if d.Sleep != nil {
		d.Sleep(ctx, dur)
		return
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (d *Dispatcher) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Send delivers one event to one hook, retrying a receiver that is down or busy (network errors, 5xx, 408,
// 429) and giving up at once on an answer that will not change (any other 4xx, a redirect).
func (d *Dispatcher) Send(ctx context.Context, h Hook, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	backoff := d.Backoff
	if backoff == nil {
		backoff = DefaultBackoff
	}
	var last error
	for attempt := 0; attempt <= len(backoff); attempt++ {
		if attempt > 0 {
			d.sleep(ctx, backoff[attempt-1])
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ts := d.now()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "offgrid-webhook/0")
		req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts.Unix(), 10))
		req.Header.Set(HeaderSignature, Sign(h.Secret, ts, body))
		req.Header.Set(HeaderEvent, ev.Type)
		req.Header.Set(HeaderDelivery, ev.ID)
		res, err := d.client().Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // the URL is in the hook already; leave it out of logs
			}
			last = fmt.Errorf("attempt %d: %v", attempt+1, err)
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		switch {
		case res.StatusCode >= 200 && res.StatusCode < 300:
			return nil
		case res.StatusCode == http.StatusRequestTimeout || res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
			last = fmt.Errorf("attempt %d: the receiver answered %d", attempt+1, res.StatusCode)
		default:
			return fmt.Errorf("the receiver answered %d, which will not change by retrying", res.StatusCode)
		}
	}
	return last
}

// Step reads one batch of new ops and delivers what hooks want. It returns how many ops it read.
func (d *Dispatcher) Step(ctx context.Context) (int, error) {
	f, err := Load(d.Path)
	if err != nil {
		return 0, err
	}
	if len(f.Hooks) == 0 {
		return 0, nil
	}
	minFrom := f.Hooks[0].From
	for _, h := range f.Hooks {
		if h.From < minFrom {
			minFrom = h.From
		}
	}
	if f.Cursor < minFrom {
		f.Cursor = minFrom // nothing before the oldest hook was added is anyone's business
	}
	ops, err := d.Source.Ops(ctx, f.Cursor, 100)
	if err != nil {
		return 0, err
	}
	for _, op := range ops {
		for _, h := range f.Hooks {
			if op.Pos <= h.From {
				continue
			}
			ev, ok := FromOp(op, h.IncludeText)
			if !ok || !h.wants(ev.Type) {
				continue
			}
			if err := d.Send(ctx, h, ev); err != nil {
				if ctx.Err() != nil {
					return 0, ctx.Err() // do not move the cursor past an event we did not try to send
				}
				d.Failed.Add(1)
				d.logf("webhook %s: event %s (%s) not delivered: %v", h.ID, ev.ID, ev.Type, err)
				continue
			}
			d.Delivered.Add(1)
		}
		f.Cursor = op.Pos
	}
	if len(ops) > 0 {
		// Merge into the file as it is now: `webhook add` may have run while we were delivering.
		cur, err := Load(d.Path)
		if err != nil {
			return len(ops), err
		}
		cur.Cursor = f.Cursor
		if err := Save(d.Path, cur); err != nil {
			return len(ops), err
		}
	}
	return len(ops), nil
}

// Run delivers events until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) error {
	poll := d.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for ctx.Err() == nil {
		n, err := d.Step(ctx)
		if err != nil && ctx.Err() == nil {
			d.logf("webhooks: %v", err)
		}
		if n == 0 || err != nil {
			d.sleep(ctx, poll)
		}
	}
	return nil
}
