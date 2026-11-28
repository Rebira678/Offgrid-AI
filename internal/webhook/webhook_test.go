package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

var t0 = time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)

func TestSignAndVerify(t *testing.T) {
	body := []byte(`{"id":"a:1"}`)
	sig := Sign("s3cret", t0, body)
	if !strings.HasPrefix(sig, "v1=") || len(sig) != 3+64 {
		t.Fatalf("%q", sig)
	}
	ts := strconv.FormatInt(t0.Unix(), 10)
	if err := Verify("s3cret", sig, ts, body, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		secret, sig, ts string
		body            []byte
		now             time.Time
	}{
		"wrong secret":    {"other", sig, ts, body, t0},
		"changed body":    {"s3cret", sig, ts, []byte(`{"id":"a:2"}`), t0},
		"changed time":    {"s3cret", sig, strconv.FormatInt(t0.Unix()+1, 10), body, t0},
		"no prefix":       {"s3cret", sig[3:], ts, body, t0},
		"empty":           {"s3cret", "", ts, body, t0},
		"bad timestamp":   {"s3cret", sig, "yesterday", body, t0},
		"too old":         {"s3cret", sig, ts, body, t0.Add(Tolerance + time.Second)},
		"from the future": {"s3cret", sig, ts, body, t0.Add(-Tolerance - time.Second)},
	} {
		if Verify(c.secret, c.sig, c.ts, c.body, c.now) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if Verify("s3cret", sig, ts, body, t0.Add(Tolerance)) != nil || Verify("s3cret", sig, ts, body, t0.Add(-Tolerance)) != nil {
		t.Error("the tolerance edge must be accepted")
	}
}

func TestValidateURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://example.com/hook": true, "http://192.168.1.5:8080/x?y=1": true, "http://localhost/h": true,
		"ftp://x/y": false, "file:///etc/passwd": false, "javascript:alert(1)": false, "http://": false, "": false,
		"https://user:pass@example.com/": false, "https://user@example.com/": false, "https://example.com/#frag": false, "example.com": false,
	} {
		if err := ValidateURL(u); (err == nil) != ok {
			t.Errorf("%q: %v", u, err)
		}
	}
}

func TestNewHook(t *testing.T) {
	h, err := NewHook("https://example.com/h", []string{" task.created ", "", "task.updated"}, false, 42, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h.ID, "wh_") || len(h.Secret) != 64 || h.From != 42 || strings.Join(h.Events, ",") != "task.created,task.updated" || !h.Created.Equal(t0) {
		t.Fatalf("%+v", h)
	}
	h2, _ := NewHook("https://example.com/h", nil, true, 0, t0)
	if h2.Secret == h.Secret || h2.ID == h.ID {
		t.Fatal("secrets and IDs must be fresh")
	}
	if all, _ := NewHook("https://example.com/h", []string{"task.created", "*"}, false, 0, t0); len(all.Events) != 0 {
		t.Fatalf("* means every type: %v", all.Events)
	}
	if _, err := NewHook("https://example.com/h", []string{"task.deleted"}, false, 0, t0); err == nil || !strings.Contains(err.Error(), "task.created") {
		t.Fatalf("an unknown event type: %v", err)
	}
	if _, err := NewHook("ftp://x", nil, false, 0, t0); err == nil {
		t.Fatal("bad url")
	}
}

func TestHooksFileIsPrivateAtomicAndRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "hooks.json")
	if f, err := Load(p); err != nil || len(f.Hooks) != 0 || f.Cursor != 0 {
		t.Fatalf("a missing file is an empty one: %+v %v", f, err)
	}
	h, _ := NewHook("https://example.com/h", nil, true, 7, t0)
	if err := Save(p, File{Hooks: []Hook{h}, Cursor: 9}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", fi.Mode().Perm())
		}
	}
	f, err := Load(p)
	if err != nil || len(f.Hooks) != 1 || f.Hooks[0].Secret != h.Secret || f.Hooks[0].ID != h.ID || f.Hooks[0].URL != h.URL || !f.Hooks[0].IncludeText || f.Hooks[0].From != 7 || f.Cursor != 9 {
		t.Fatalf("%+v %v", f, err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 1 {
		t.Fatalf("temp files left behind: %v", ents)
	}
	os.WriteFile(p, []byte("not json"), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "not a hooks file") {
		t.Fatalf("%v", err)
	}
}

// ---------------------------------------------------------------- events from a real store

type env struct {
	t   *testing.T
	st  *sqlstore.Store
	clk time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, clk: t0}
	st, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), "n.db"), sqlstore.Options{NodeID: "site", Now: func() time.Time { e.clk = e.clk.Add(time.Second); return e.clk }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	return e
}

func (e *env) ops() []sqlstore.Op {
	ops, err := e.st.Ops(context.Background(), 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	return ops
}

func TestEventsFromOpsCarryIDsAndChangedValuesButTextOnlyOnRequest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tk, _, _ := e.st.Create(ctx, task.NewTask{Title: "fix the secret pump", Site: "farm", Priority: task.PriorityHigh})
	done := task.StatusDone
	e.st.Update(ctx, tk.ID, task.Changes{Status: &done})
	e.st.AddNote(ctx, tk.ID, "the code is 4411")
	lo := task.PriorityLow
	sg, _, _ := e.st.Suggest(ctx, task.Draft{Task: tk.ID, Priority: &lo, By: "model", Key: "k"})
	e.st.Resolve(ctx, sg.ID, false)
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, bytes.Repeat([]byte{1}, 500)...)
	e.st.AddAttachment(ctx, tk.ID, bytes.NewReader(jpeg), int64(len(jpeg)), "image/jpeg", "gate-code.jpg")

	var plain, text []Event
	for _, op := range e.ops() {
		if ev, ok := FromOp(op, false); ok {
			plain = append(plain, ev)
		}
		if ev, ok := FromOp(op, true); ok {
			text = append(text, ev)
		}
	}
	var types []string
	for _, ev := range plain {
		types = append(types, ev.Type)
	}
	if got := strings.Join(types, ","); got != "task.created,task.updated,task.note_added,suggestion.made,suggestion.resolved,attachment.added" {
		t.Fatalf("%s", got)
	}
	blob, _ := json.Marshal(plain)
	for _, secret := range []string{"secret pump", "farm", "4411", "gate-code"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("text %q left the node without include_text: %s", secret, blob)
		}
	}
	tb, _ := json.Marshal(text)
	for _, want := range []string{"fix the secret pump", "farm", "4411", "gate-code.jpg"} {
		if !strings.Contains(string(tb), want) {
			t.Errorf("include_text lacks %q: %s", want, tb)
		}
	}
	if plain[0].Task != tk.ID || plain[0].Data["priority"] != "high" || plain[1].Data["status"] != "done" || plain[1].Data["priority"] != nil {
		t.Fatalf("%+v %+v", plain[0], plain[1])
	}
	if plain[3].Task != tk.ID || plain[3].Data["by"] != "model" || plain[4].Data["resolution"] != "dismissed" || plain[4].Data["suggestion"] == "" {
		t.Fatalf("%+v %+v", plain[3], plain[4])
	}
	if plain[5].Task != tk.ID || plain[5].Data["type"] != "image/jpeg" || plain[5].Data["size"] != int64(len(jpeg)) || plain[5].Data["hash"] == "" {
		t.Fatalf("%+v", plain[5])
	}
	seen := map[string]bool{}
	for _, ev := range plain {
		if !strings.HasPrefix(ev.ID, "site:") || seen[ev.ID] || ev.Node != "site" || ev.At.IsZero() {
			t.Fatalf("%+v", ev)
		}
		seen[ev.ID] = true
	}
	// an op that is not an event
	if _, ok := FromOp(sqlstore.Op{Kind: "member.add", Payload: []byte(`{}`)}, true); ok {
		t.Error("membership is not an event")
	}
	if _, ok := FromOp(sqlstore.Op{Kind: sqlstore.KindTaskCreated, Payload: []byte(`nope`)}, true); ok {
		t.Error("a payload that is not JSON")
	}
}

// ---------------------------------------------------------------- delivery

type receiver struct {
	mu     sync.Mutex
	got    []rec
	status func(n int) int // answer for the n-th request
	srv    *httptest.Server
}

type rec struct {
	body []byte
	hdr  http.Header
}

func newReceiver(t *testing.T, status func(n int) int) *receiver {
	r := &receiver{status: status}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, rec{b, req.Header.Clone()})
		n := len(r.got)
		r.mu.Unlock()
		w.WriteHeader(r.status(n))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.got) }

func ok200(int) int { return 200 }

func noSleep(context.Context, time.Duration) {}

func newDispatcher(e *env, path string) *Dispatcher {
	return &Dispatcher{Source: e.st, Path: path, Now: func() time.Time { return t0 }, Sleep: noSleep}
}

func TestADeliveryIsSignedAndVerifiable(t *testing.T) {
	e := newEnv(t)
	rc := newReceiver(t, ok200)
	p := filepath.Join(t.TempDir(), "h.json")
	h, _ := NewHook(rc.srv.URL, nil, false, 0, t0)
	Save(p, File{Hooks: []Hook{h}})
	e.st.Create(context.Background(), task.NewTask{Title: "x", Priority: task.PriorityLow})
	d := newDispatcher(e, p)
	if n, err := d.Step(context.Background()); err != nil || n == 0 {
		t.Fatalf("%d %v", n, err)
	}
	if rc.count() != 1 {
		t.Fatalf("%d deliveries", rc.count())
	}
	g := rc.got[0]
	if err := Verify(h.Secret, g.hdr.Get(HeaderSignature), g.hdr.Get(HeaderTimestamp), g.body, t0); err != nil {
		t.Fatal(err)
	}
	var ev Event
	json.Unmarshal(g.body, &ev)
	if g.hdr.Get(HeaderEvent) != "task.created" || g.hdr.Get(HeaderDelivery) != ev.ID || ev.Type != "task.created" || g.hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("%v %+v", g.hdr, ev)
	}
	if d.Delivered.Load() != 1 || d.Failed.Load() != 0 {
		t.Fatal("counters")
	}
}

func TestAHookNeverSeesWhatHappenedBeforeItWasAddedAndOnlyGetsItsEvents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.st.Create(ctx, task.NewTask{Title: "before", Priority: task.PriorityLow})
	before := e.ops()
	from := before[len(before)-1].Pos
	rc := newReceiver(t, ok200)
	p := filepath.Join(t.TempDir(), "h.json")
	h, _ := NewHook(rc.srv.URL, []string{"task.note_added"}, false, from, t0)
	Save(p, File{Hooks: []Hook{h}})
	tk, _, _ := e.st.Create(ctx, task.NewTask{Title: "after", Priority: task.PriorityLow})
	e.st.AddNote(ctx, tk.ID, "n")
	done := task.StatusDone
	e.st.Update(ctx, tk.ID, task.Changes{Status: &done})
	d := newDispatcher(e, p)
	d.Step(ctx)
	if rc.count() != 1 {
		t.Fatalf("%d deliveries; only the note, made after the hook, matches", rc.count())
	}
	var ev Event
	json.Unmarshal(rc.got[0].body, &ev)
	if ev.Type != "task.note_added" {
		t.Fatal(ev.Type)
	}
	// the cursor moved: a second step delivers nothing
	d.Step(ctx)
	if rc.count() != 1 {
		t.Fatalf("redelivered: %d", rc.count())
	}
	f, _ := Load(p)
	if last := e.ops(); f.Cursor != last[len(last)-1].Pos {
		t.Fatalf("cursor %d", f.Cursor)
	}
}

func TestTheCursorSurvivesARestartAndAnAddDuringDeliveryIsKept(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var p string
	var added Hook
	rc := newReceiver(t, ok200)
	p = filepath.Join(t.TempDir(), "h.json")
	h, _ := NewHook(rc.srv.URL, nil, false, 0, t0)
	Save(p, File{Hooks: []Hook{h}})
	e.st.Create(ctx, task.NewTask{Title: "a", Priority: task.PriorityLow})
	// while the dispatcher is delivering, the owner adds another hook
	added, _ = NewHook("https://example.com/second", nil, false, 0, t0)
	d := newDispatcher(e, p)
	d.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f, _ := Load(p)
		f.Hooks = append(f.Hooks, added)
		Save(p, f)
		return http.DefaultTransport.RoundTrip(r)
	})}
	d.Step(ctx)
	f, _ := Load(p)
	if len(f.Hooks) != 2 || f.Cursor == 0 {
		t.Fatalf("the hook added during delivery was lost, or the cursor was: %+v", f)
	}
	// a new dispatcher (a restart) does not resend
	d2 := newDispatcher(e, p)
	d2.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, io.EOF })}
	n := rc.count()
	d2.Step(ctx)
	if rc.count() != n {
		t.Fatal("resent after a restart")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRetriesAreForTroubleThatPassesNotForAnswersThatWillNotChange(t *testing.T) {
	for _, c := range []struct {
		name     string
		status   func(int) int
		wantReqs int
		wantErr  bool
	}{
		{"ok first time", ok200, 1, false},
		{"202 is fine", func(int) int { return 202 }, 1, false},
		{"500 then ok", func(n int) int {
			if n == 1 {
				return 500
			}
			return 200
		}, 2, false},
		{"429 twice then ok", func(n int) int {
			if n < 3 {
				return 429
			}
			return 200
		}, 3, false},
		{"408 then ok", func(n int) int {
			if n == 1 {
				return 408
			}
			return 200
		}, 2, false},
		{"always 503", func(int) int { return 503 }, 3, true},
		{"404 is final", func(int) int { return 404 }, 1, true},
		{"401 is final", func(int) int { return 401 }, 1, true},
		{"301 is final and not followed", func(int) int { return 301 }, 1, true},
		{"300 is not success", func(int) int { return 300 }, 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			rc := newReceiver(t, c.status)
			d := newDispatcher(e, "")
			var waits []time.Duration
			d.Sleep = func(_ context.Context, w time.Duration) { waits = append(waits, w) }
			h, _ := NewHook(rc.srv.URL, nil, false, 0, t0)
			err := d.Send(context.Background(), h, Event{ID: "a:1", Type: "ping"})
			if (err != nil) != c.wantErr || rc.count() != c.wantReqs {
				t.Fatalf("err %v, %d requests (want %d)", err, rc.count(), c.wantReqs)
			}
			if c.wantReqs == 3 && (len(waits) != 2 || waits[0] != time.Second || waits[1] != 4*time.Second) {
				t.Fatalf("waits %v", waits)
			}
		})
	}
}

func TestADeadReceiverDoesNotStopOthersAndTheCursorMovesOn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dead := newReceiver(t, func(int) int { return 500 })
	live := newReceiver(t, ok200)
	p := filepath.Join(t.TempDir(), "h.json")
	h1, _ := NewHook(dead.srv.URL, nil, false, 0, t0)
	h2, _ := NewHook(live.srv.URL, nil, false, 0, t0)
	Save(p, File{Hooks: []Hook{h1, h2}})
	e.st.Create(ctx, task.NewTask{Title: "a", Priority: task.PriorityLow})
	d := newDispatcher(e, p)
	var logs []string
	d.Logf = func(f string, a ...any) { logs = append(logs, f) }
	d.Step(ctx)
	if live.count() != 1 || dead.count() != 3 || d.Failed.Load() != 1 || d.Delivered.Load() != 1 || len(logs) != 1 {
		t.Fatalf("live %d dead %d failed %d delivered %d logs %d", live.count(), dead.count(), d.Failed.Load(), d.Delivered.Load(), len(logs))
	}
	if f, _ := Load(p); f.Cursor == 0 {
		t.Fatal("the cursor must move past an event that could not be delivered, or one dead receiver stops the world")
	}
}

func TestAnUnreachableReceiverIsAFailureWithoutTheURLInTheError(t *testing.T) {
	e := newEnv(t)
	d := newDispatcher(e, "")
	h, _ := NewHook("http://127.0.0.1:1/secret-path-token", nil, false, 0, t0)
	err := d.Send(context.Background(), h, Event{ID: "a:1", Type: "ping"})
	if err == nil || strings.Contains(err.Error(), "secret-path-token") {
		t.Fatalf("%v", err)
	}
}

func TestStoppingMidDeliveryDoesNotLoseTheEvent(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	rc := newReceiver(t, func(int) int { return 500 })
	p := filepath.Join(t.TempDir(), "h.json")
	h, _ := NewHook(rc.srv.URL, nil, false, 0, t0)
	Save(p, File{Hooks: []Hook{h}})
	e.st.Create(context.Background(), task.NewTask{Title: "a", Priority: task.PriorityLow})
	d := newDispatcher(e, p)
	d.Sleep = func(context.Context, time.Duration) { cancel() }
	if _, err := d.Step(ctx); err == nil {
		t.Fatal("a cancelled step must say so")
	}
	if f, _ := Load(p); f.Cursor != 0 {
		t.Fatalf("the cursor moved past an event that was never given up on: %d", f.Cursor)
	}
}

func TestRunDeliversNewEventsAndStopsWithItsContext(t *testing.T) {
	e := newEnv(t)
	rc := newReceiver(t, ok200)
	p := filepath.Join(t.TempDir(), "h.json")
	h, _ := NewHook(rc.srv.URL, nil, false, 0, t0)
	Save(p, File{Hooks: []Hook{h}})
	d := newDispatcher(e, p)
	d.Poll = 10 * time.Millisecond
	d.Sleep = nil // the real timer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	e.st.Create(context.Background(), task.NewTask{Title: "later", Priority: task.PriorityLow})
	deadline := time.Now().Add(5 * time.Second)
	for rc.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rc.count() != 1 {
		t.Fatalf("%d", rc.count())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestNoHooksMeansNoWork(t *testing.T) {
	e := newEnv(t)
	d := newDispatcher(e, filepath.Join(t.TempDir(), "none.json"))
	e.st.Create(context.Background(), task.NewTask{Title: "a", Priority: task.PriorityLow})
	if n, err := d.Step(context.Background()); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
}

func TestRedirectsAreNotFollowedAndTheClientHasATimeout(t *testing.T) {
	e := newEnv(t)
	target := newReceiver(t, ok200)
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	d := newDispatcher(e, "")
	h, _ := NewHook(redir.URL, nil, false, 0, t0)
	if err := d.Send(context.Background(), h, Event{ID: "a:1", Type: "ping"}); err == nil || target.count() != 0 {
		t.Fatalf("a redirect must not be followed (SSRF): %v, %d", err, target.count())
	}
	if c := (&Dispatcher{}).client(); c.Timeout <= 0 || c.Timeout > 30*time.Second {
		t.Fatalf("timeout %v", c.Timeout)
	}
}

func TestEachHookGetsEverythingAfterItsOwnStartEvenWhenAnotherStartedLater(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.st.Create(ctx, task.NewTask{Title: "one", Priority: task.PriorityLow})
	mid := e.ops()[len(e.ops())-1].Pos
	e.st.Create(ctx, task.NewTask{Title: "two", Priority: task.PriorityLow})
	old, new := newReceiver(t, ok200), newReceiver(t, ok200)
	p := filepath.Join(t.TempDir(), "h.json")
	h1, _ := NewHook(old.srv.URL, nil, false, 0, t0)
	h2, _ := NewHook(new.srv.URL, nil, false, mid, t0)
	Save(p, File{Hooks: []Hook{h1, h2}})
	newDispatcher(e, p).Step(ctx)
	if old.count() != 2 || new.count() != 1 {
		t.Fatalf("old %d new %d", old.count(), new.count())
	}
}

func TestTheEventAtExactlyTheStartPositionBelongsToTheOldWorld(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.st.Create(ctx, task.NewTask{Title: "one", Priority: task.PriorityLow})
	last := e.ops()[len(e.ops())-1].Pos
	rc := newReceiver(t, ok200)
	p := filepath.Join(t.TempDir(), "h.json")
	h, _ := NewHook(rc.srv.URL, nil, false, last, t0)
	Save(p, File{Hooks: []Hook{h}})
	newDispatcher(e, p).Step(ctx)
	if rc.count() != 0 {
		t.Fatal("the event at the hook's own From position was delivered")
	}
}
