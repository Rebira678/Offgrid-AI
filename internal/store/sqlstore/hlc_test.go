package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"offgrid/internal/hlc"
	"offgrid/internal/task"
)

func create(t *testing.T, s *Store, title string) task.Task {
	t.Helper()
	tk, _, err := s.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func allOps(t *testing.T, s *Store) []Op {
	t.Helper()
	ops, err := s.Ops(context.Background(), 0, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func TestOpsAreStampedByTheHybridClockWithACounterWhenTimeStandsStill(t *testing.T) {
	s, clk := freshStore(t) // the physical clock never moves
	for i := 0; i < 5; i++ {
		create(t, s, fmt.Sprintf("task %d", i))
	}
	ops := allOps(t, s)
	if len(ops) != 5 {
		t.Fatalf("%d ops", len(ops))
	}
	for i, op := range ops {
		if !op.TS.Equal(clk.t) || op.Counter != uint32(i) {
			t.Errorf("op %d stamped %v#%d, want %v#%d", i, op.TS, op.Counter, clk.t, i)
		}
		if i > 0 && !op.Stamp().After(ops[i-1].Stamp()) {
			t.Errorf("op %d is not after op %d", i, i-1)
		}
	}
}

func TestOpTimestampsKeepIncreasingWhenThePhysicalClockGoesBackwards(t *testing.T) {
	s, clk := freshStore(t)
	create(t, s, "a")
	clk.tick(10 * time.Minute)
	create(t, s, "b")
	clk.tick(-3 * time.Hour) // the clock is set back
	create(t, s, "c")
	clk.tick(3*time.Hour + 20*time.Minute) // and then forward again
	create(t, s, "d")

	ops := allOps(t, s)
	for i := 1; i < len(ops); i++ {
		if !ops[i].Stamp().After(ops[i-1].Stamp()) {
			t.Errorf("op %d (%v) is not after op %d (%v)", i, ops[i].Stamp(), i-1, ops[i-1].Stamp())
		}
	}
	// "c" was recorded while the clock was behind: it keeps the larger wall
	if !ops[2].TS.Equal(ops[1].TS) {
		t.Errorf("an op recorded while the clock was behind keeps the latest wall: %v vs %v", ops[2].TS, ops[1].TS)
	}
	// and the view shows times that never go backwards either
	tasks, _ := s.List(context.Background(), task.Filter{})
	for i := 1; i < len(tasks); i++ {
		if tasks[i].CreatedAt.Before(tasks[i-1].CreatedAt) {
			t.Errorf("task %d was created before task %d in the view", i, i-1)
		}
	}
}

func TestARestartWithAClockThatIsBehindNeverIssuesAnOlderStamp(t *testing.T) {
	path := newDB(t)
	clk := &clock{t: epoch}
	s, err := Open(context.Background(), path, Options{NodeID: "node-a", Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	create(t, s, "before")
	create(t, s, "before too")
	last := allOps(t, s)[1].Stamp()
	s.Close()

	back := &clock{t: epoch.Add(-48 * time.Hour)} // the device lost its clock
	s2 := open(t, path, Options{Now: back.now})
	create(t, s2, "after")
	ops := allOps(t, s2)
	if got := ops[2].Stamp(); !got.After(last) {
		t.Errorf("after a restart the op is stamped %v, not after %v", got, last)
	}
	info, err := s2.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.NewestStamp != ops[2].Stamp() {
		t.Errorf("Info.NewestStamp = %v", info.NewestStamp)
	}
	if info.ClockAhead < 47*time.Hour || info.ClockAhead > 49*time.Hour {
		t.Errorf("Info.ClockAhead = %v: the log is about 48h ahead of this clock", info.ClockAhead)
	}
}

func TestCounterAndWallArePersistedExactly(t *testing.T) {
	s, _ := freshStore(t)
	s.Clock().Restore(hlc.Timestamp{Wall: epoch.UnixNano() + 5, Counter: 4_000_000_000})
	create(t, s, "x")
	op := allOps(t, s)[0]
	if op.TS.UnixNano() != epoch.UnixNano()+5 || op.Counter != 4_000_000_001 {
		t.Errorf("%v #%d", op.TS, op.Counter)
	}
	// and the JSON shows both parts
	b, _ := jsonOf(op)
	if !strings.Contains(b, `"hlc_counter":4000000001`) {
		t.Errorf("%s", b)
	}
}

func TestViewTimesAreTheClocksWallAndRebuildAgrees(t *testing.T) {
	s, clk := freshStore(t)
	a := create(t, s, "a")
	clk.tick(time.Second)
	b := create(t, s, "b")
	if !a.CreatedAt.Equal(epoch) || !b.CreatedAt.Equal(epoch.Add(time.Second)) {
		t.Errorf("created %v %v", a.CreatedAt, b.CreatedAt)
	}
	rep, err := s.Verify(context.Background())
	if err != nil || rep.Ops != 2 {
		t.Fatalf("verify: %v %v", rep, err)
	}
	// ten ops in the same instant: the counter orders them, the view shows one time
	for i := 0; i < 10; i++ {
		create(t, s, fmt.Sprintf("same instant %d", i))
	}
	if _, err := s.Verify(context.Background()); err != nil {
		t.Fatalf("rebuilding from the log must equal the live view: %v", err)
	}
}

func TestSyncLayerFoldsInARemoteStampSoTheNextOpIsAfterIt(t *testing.T) {
	s, clk := freshStore(t)
	create(t, s, "mine")
	remote := hlc.Timestamp{Wall: epoch.Add(10 * time.Minute).UnixNano(), Counter: 7} // a peer whose clock runs fast
	if _, err := s.Clock().Update(remote); err != nil {
		t.Fatal(err)
	}
	clk.tick(time.Second) // my own clock is still 10 minutes behind
	create(t, s, "edited after seeing the peer's change")
	ops := allOps(t, s)
	if !ops[1].Stamp().After(remote) {
		t.Errorf("an edit made after receiving %v is stamped %v: the slow clock would lose", remote, ops[1].Stamp())
	}
}

func TestMaxDriftOptionReachesTheClock(t *testing.T) {
	clk := &clock{t: epoch}
	s := open(t, newDB(t), Options{NodeID: "n", Now: clk.now, MaxDrift: time.Minute})
	before := s.Clock().Last()
	_, err := s.Clock().Update(hlc.Timestamp{Wall: epoch.Add(2 * time.Minute).UnixNano()})
	if !errors.Is(err, hlc.ErrDrift) {
		t.Fatalf("%v", err)
	}
	if s.Clock().Last() != before {
		t.Error("a refused timestamp moved the clock")
	}
}

// ---- the schema upgrade ---------------------------------------------------------

func TestUpgradeFromAnOldLogKeepsEveryOpAndItsTimes(t *testing.T) {
	ctx := context.Background()
	path := newDB(t)
	raw, err := sql.Open(driverName, dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	if err := migrate(ctx, raw, migrations[:2]); err != nil { // a Day 6 database
		t.Fatal(err)
	}
	legacy := []struct {
		seq int
		ts  time.Time
		pay string
	}{
		{1, epoch, `{"id":"T1","title":"Leaking pump","site":"4","priority":"high"}`},
		{2, epoch.Add(time.Hour).Add(123), `{"id":"T2","title":"Replace filter","site":"","priority":"medium"}`},
	}
	for _, l := range legacy {
		if _, err := raw.Exec(`INSERT INTO ops (node_id, seq, ts, kind, payload) VALUES ('node-a', ?, ?, 'task.created', ?)`, l.seq, l.ts.UnixNano(), l.pay); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	clk := &clock{t: epoch.Add(30 * time.Minute)} // the clock is behind the newest legacy op
	s := open(t, path, Options{Now: clk.now})
	ops := allOps(t, s)
	if len(ops) != 2 {
		t.Fatalf("%d ops", len(ops))
	}
	for i, op := range ops {
		if op.Counter != 0 || !op.TS.Equal(legacy[i].ts) {
			t.Errorf("legacy op %d changed: %v #%d", i, op.TS, op.Counter)
		}
	}
	tasks, _ := s.List(ctx, task.Filter{})
	if len(tasks) != 2 || !tasks[1].CreatedAt.Equal(legacy[1].ts) {
		t.Errorf("the view of legacy ops must keep its exact (nanosecond) times: %+v", tasks)
	}
	create(t, s, "new")
	ops = allOps(t, s)
	if !ops[2].Stamp().After(ops[1].Stamp()) {
		t.Errorf("a new op %v must be after the newest legacy op %v", ops[2].Stamp(), ops[1].Stamp())
	}
	if _, err := s.Verify(ctx); err != nil {
		t.Errorf("verify after upgrade: %v", err)
	}
}

func TestOpsStayAppendOnlyAndTheCounterIsChecked(t *testing.T) {
	s, _ := freshStore(t)
	create(t, s, "x")
	if _, err := s.db.Exec(`UPDATE ops SET counter = 99`); err == nil {
		t.Error("the counter of a recorded op could be rewritten")
	}
	if _, err := s.db.Exec(`INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES ('z', 1, 1, -1, 'k', '{}')`); err == nil {
		t.Error("a negative counter was accepted")
	}
	if _, err := s.db.Exec(`INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES ('z', 1, 1, 4294967296, 'k', '{}')`); err == nil {
		t.Error("a counter beyond 32 bits was accepted")
	}
}

func TestNewestStampLookupUsesTheIndex(t *testing.T) {
	s, _ := freshStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT ts, counter FROM ops ORDER BY ts DESC, counter DESC LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var a, b, c int
		var d string
		rows.Scan(&a, &b, &c, &d)
		plan = append(plan, d)
	}
	if !strings.Contains(strings.Join(plan, "|"), "ops_hlc") {
		t.Errorf("starting up would scan the whole log: %v", plan)
	}
}

func jsonOf(op Op) (string, error) {
	b, err := json.Marshal(op)
	return string(b), err
}

// The newest stamp is the largest (wall, counter) pair, not the last row and
// not the largest counter. Rows are inserted out of order on purpose, the way
// ops arriving from another replica will be.
func TestNewestStampIsTheLargestWallThenCounterNotTheLastRow(t *testing.T) {
	path := newDB(t)
	clk := &clock{t: epoch}
	s := open(t, path, Options{NodeID: "node-a", Now: clk.now})
	create(t, s, "seed") // makes the schema and one op
	wall := epoch.UnixNano()
	rows := []struct {
		wall    int64
		counter uint32
	}{{wall + 500, 2}, {wall + 500, 7}, {wall + 500, 3}, {wall + 100, 900}}
	for i, r := range rows {
		if _, err := s.db.Exec(`INSERT INTO ops (node_id, seq, ts, counter, kind, payload) VALUES ('node-b', ?, ?, ?, 'task.note_added', '{}')`,
			i+1, r.wall, r.counter); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.newestStamp(context.Background(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	if want := (hlc.Timestamp{Wall: wall + 500, Counter: 7}); got != want {
		t.Errorf("newest stamp %v, want %v", got, want)
	}
}
