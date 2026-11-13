package sqlstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"offgrid/internal/task"
)

// chained returns a store with n tasks created by one replica and a couple of edits.
func chained(t *testing.T, node string, n int) (*Store, *clock) {
	t.Helper()
	s, clk := replicaAt(t, node, epoch)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		clk.tick(time.Minute)
		got, _, err := s.Create(ctx, task.NewTask{Title: fmt.Sprintf("task %d", i+1), Priority: task.PriorityLow})
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 1 {
			clk.tick(time.Second)
			done := task.StatusDone
			if _, _, err := s.Update(ctx, got.ID, task.Changes{Status: &done}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s, clk
}

// tamper does what someone with the file would: switch off the append-only
// triggers and change the log directly.
func tamper(t *testing.T, s *Store, queries ...string) {
	t.Helper()
	for _, trig := range []string{"ops_append_only_update", "ops_append_only_delete", "op_chain_append_only_update", "op_chain_append_only_delete"} {
		if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS ` + trig); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func wantTamper(t *testing.T, err error, node string, seq int64, reasonHas string) {
	t.Helper()
	var te *TamperError
	if !errors.Is(err, ErrTampered) || !errors.As(err, &te) {
		t.Fatalf("err = %v, want a TamperError", err)
	}
	if te.Node != node || te.Seq != seq || !strings.Contains(te.Reason, reasonHas) {
		t.Errorf("tamper at %s/%d %q, want %s/%d containing %q", te.Node, te.Seq, te.Reason, node, seq, reasonHas)
	}
}

func TestOpHashCoversEveryFieldAndIgnoresJSONWhitespace(t *testing.T) {
	base := Op{NodeID: "laptop", Seq: 3, TS: epoch, Counter: 2, Kind: KindTaskCreated, Payload: json.RawMessage(`{"id":"T1","title":"a b"}`)}
	h := OpHash(base, GenesisHash)
	if len(h) != 64 {
		t.Fatalf("hash %q is not 64 hex digits", h)
	}
	if OpHash(base, GenesisHash) != h {
		t.Fatal("not deterministic")
	}
	for name, mod := range map[string]func(Op) (Op, string){
		"node":    func(o Op) (Op, string) { o.NodeID = "phone"; return o, GenesisHash },
		"seq":     func(o Op) (Op, string) { o.Seq = 4; return o, GenesisHash },
		"ts":      func(o Op) (Op, string) { o.TS = o.TS.Add(time.Nanosecond); return o, GenesisHash },
		"counter": func(o Op) (Op, string) { o.Counter = 3; return o, GenesisHash },
		"kind":    func(o Op) (Op, string) { o.Kind = KindTaskUpdated; return o, GenesisHash },
		"payload": func(o Op) (Op, string) {
			o.Payload = json.RawMessage(`{"id":"T1","title":"a c"}`)
			return o, GenesisHash
		},
		"previous": func(o Op) (Op, string) { return o, strings.Repeat("1", 64) },
	} {
		o, prev := mod(base)
		if OpHash(o, prev) == h {
			t.Errorf("changing the %s did not change the hash", name)
		}
	}
	spaced := base
	spaced.Payload = json.RawMessage("{ \"id\": \"T1\",\n  \"title\": \"a b\" }")
	if OpHash(spaced, GenesisHash) != h {
		t.Error("JSON whitespace must not matter: a peer may re-indent the payload")
	}
	// Text cannot be moved across a field boundary to keep the same hash:
	// counter 1 + kind "2x" and counter 12 + kind "x" are the same characters in a row.
	a, b := base, base
	a.Counter, a.Kind = 1, "2x"
	b.Counter, b.Kind = 12, "x"
	if OpHash(a, GenesisHash) == OpHash(b, GenesisHash) {
		t.Error("fields are not length-prefixed: counter 1 + kind 2x collides with counter 12 + kind x")
	}
}

func TestEveryOpIsChainedToTheOneBeforeFromTheSameReplica(t *testing.T) {
	s, _ := chained(t, "laptop", 4)
	ctx := context.Background()
	ops, err := s.Ops(ctx, 0, 100)
	if err != nil || len(ops) != 6 {
		t.Fatalf("%d ops, err %v", len(ops), err)
	}
	prev := GenesisHash
	for _, op := range ops {
		if op.Hash != OpHash(op, prev) {
			t.Fatalf("op %d: stored hash is not hash(op, previous)", op.Seq)
		}
		prev = op.Hash
	}
	rep, err := s.VerifyChain(ctx)
	if err != nil || rep.Ops != 6 || rep.Retro != 0 || len(rep.Heads) != 1 || rep.Heads[0] != (Head{"laptop", 6, prev}) {
		t.Errorf("report %+v err %v", rep, err)
	}
	if _, err := s.Verify(ctx); err != nil {
		t.Errorf("Verify: %v", err)
	}
	// A restart does not change it.
	s2 := open(t, s.path(), Options{})
	if h2, _ := s2.Heads(ctx); len(h2) != 1 || h2[0].Hash != prev {
		t.Errorf("heads after reopening: %+v", h2)
	}
}

func TestAnEditedOpIsNamedExactlyAndTheEarliestOneWins(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		q    string
		seq  int64
		what string
	}{
		"a word in a title":    {`UPDATE ops SET payload = replace(payload, 'task 1', 'task 9') WHERE node_id='laptop' AND seq = 1`, 1, "was changed"},
		"a status":             {`UPDATE ops SET payload = replace(payload, 'done', 'open') WHERE node_id='laptop' AND seq = 3`, 3, "was changed"},
		"a timestamp":          {`UPDATE ops SET ts = ts + 1 WHERE node_id='laptop' AND seq = 4`, 4, "was changed"},
		"a counter":            {`UPDATE ops SET counter = 7 WHERE node_id='laptop' AND seq = 2`, 2, "was changed"},
		"a kind":               {`UPDATE ops SET kind = 'task.removed' WHERE node_id='laptop' AND seq = 5`, 5, "was changed"},
		"the last op":          {`UPDATE ops SET payload = replace(payload, 'done', 'open') WHERE node_id='laptop' AND seq = 6`, 6, "was changed"},
		"an op deleted":        {`DELETE FROM ops WHERE node_id='laptop' AND seq = 2`, 2, "missing"},
		"a chain entry gone":   {`DELETE FROM op_chain WHERE node_id='laptop' AND seq = 3`, 3, "no entry"},
		"a chain link edited":  {`UPDATE op_chain SET prev = '` + strings.Repeat("a", 64) + `' WHERE node_id='laptop' AND seq = 4`, 4, "not linked"},
		"a chain hash edited":  {`UPDATE op_chain SET hash = '` + strings.Repeat("b", 64) + `' WHERE node_id='laptop' AND seq = 2`, 2, "was changed"},
		"an op removed at end": {`DELETE FROM ops WHERE node_id='laptop' AND seq = 6`, 6, "no op"},
	} {
		s, _ := chained(t, "laptop", 4)
		tamper(t, s, c.q)
		_, err := s.VerifyChain(ctx)
		t.Run(name, func(t *testing.T) { wantTamper(t, err, "laptop", c.seq, c.what) })
	}
	// Edit two ops: the earlier one is reported.
	s, _ := chained(t, "laptop", 4)
	tamper(t, s, `UPDATE ops SET payload = replace(payload, 'done', 'open') WHERE seq = 6`, `UPDATE ops SET payload = replace(payload, 'task 2', 'task 8') WHERE seq = 2`)
	_, err := s.VerifyChain(ctx)
	wantTamper(t, err, "laptop", 2, "was changed")
}

func TestVerifyReportsATamperedOpBeforeTheViewDifferencesItCauses(t *testing.T) {
	s, _ := chained(t, "laptop", 4)
	ctx := context.Background()
	tamper(t, s, `UPDATE ops SET payload = replace(payload, 'task 1', 'task 9') WHERE seq = 1`)
	_, err := s.Verify(ctx)
	wantTamper(t, err, "laptop", 1, "was changed")
	if errors.Is(err, ErrDiverged) {
		t.Error("a tampered log must not be reported as a mere stale view")
	}
	// Editing only the view (the tasks table) is still the old, repairable case.
	s2, _ := chained(t, "laptop", 4)
	tamper(t, s2, `UPDATE tasks SET title = 'edited in the view' WHERE id LIKE 'T1.%'`)
	if _, err := s2.Verify(ctx); !errors.Is(err, ErrDiverged) || errors.Is(err, ErrTampered) {
		t.Errorf("view-only edit: err = %v, want ErrDiverged", err)
	}
}

// The limit, stated as a test: someone who rewrites an op AND recomputes the
// chain from there on cannot be seen by looking at this file alone. A head
// saved earlier, somewhere else, sees it.
func TestARewrittenChainPassesAloneButNotAgainstASavedHead(t *testing.T) {
	ctx := context.Background()
	s, _ := chained(t, "laptop", 5)
	saved, err := s.Heads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Change op 2's payload, then redo every hash from op 2 on, exactly as a careful attacker would.
	tamper(t, s, `UPDATE ops SET payload = replace(payload, 'task 1', 'task 9') WHERE seq = 1`)
	ops, _ := s.Ops(ctx, 0, 100)
	prev := GenesisHash
	for _, op := range ops {
		h := OpHash(op, prev)
		if _, err := s.db.Exec(`UPDATE op_chain SET prev = ?, hash = ? WHERE node_id = ? AND seq = ?`, prev, h, op.NodeID, op.Seq); err != nil {
			t.Fatal(err)
		}
		prev = h
	}
	if _, err := s.VerifyChain(ctx); err != nil {
		t.Fatalf("a fully rewritten chain is self-consistent, so VerifyChain cannot object: %v", err)
	}
	err = s.VerifyAnchors(ctx, saved)
	wantTamper(t, err, "laptop", saved[0].Seq, "rewritten")
}

func TestASavedHeadCatchesTheLogLosingItsNewestOps(t *testing.T) {
	ctx := context.Background()
	s, _ := chained(t, "laptop", 4)
	saved, _ := s.Heads(ctx)
	if err := s.VerifyAnchors(ctx, saved); err != nil {
		t.Fatalf("an untouched log must agree with its own head: %v", err)
	}
	// Cut the tail cleanly: ops and their chain entries, so the chain still verifies.
	tamper(t, s, `DELETE FROM ops WHERE seq > 4`, `DELETE FROM op_chain WHERE seq > 4`)
	if _, err := s.VerifyChain(ctx); err != nil {
		t.Fatalf("a cleanly truncated log is a valid chain: %v", err)
	}
	wantTamper(t, s.VerifyAnchors(ctx, saved), "laptop", saved[0].Seq, "removed")
	// An old anchor still holds on a log that only grew.
	g, _ := chained(t, "laptop", 4)
	early, _ := g.Heads(ctx)
	g.Create(ctx, task.NewTask{Title: "later", Priority: task.PriorityLow})
	if err := g.VerifyAnchors(ctx, early); err != nil {
		t.Errorf("an earlier head must still be in a log that grew: %v", err)
	}
	// An unknown replica in the anchors is "not here".
	wantTamper(t, g.VerifyAnchors(ctx, []Head{{"ghost", 1, strings.Repeat("c", 64)}}), "ghost", 1, "removed")
}

func TestTheChainTableIsAppendOnlyLikeTheLog(t *testing.T) {
	s, _ := chained(t, "laptop", 2)
	for name, q := range map[string]string{
		"update": `UPDATE op_chain SET hash = 'x'`,
		"delete": `DELETE FROM op_chain`,
	} {
		if _, err := s.db.Exec(q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want the append-only refusal", name, err)
		}
	}
}

func TestSingleByteFlipsAreAlwaysFound(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(27))
	s, _ := chained(t, "laptop", 6)
	n := countOps(t, s)
	for i := 0; i < 60; i++ {
		seq := int64(rng.Intn(n) + 1)
		var payload string
		if err := s.db.QueryRow(`SELECT payload FROM ops WHERE seq = ?`, seq).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		pos := rng.Intn(len(payload))
		flipped := []byte(payload)
		// Only letters and digits: the log refuses text that is not JSON, and a
		// careful editor would keep it valid.
		if c := flipped[pos]; c >= 'a' && c <= 'y' || c >= '0' && c <= '8' {
			flipped[pos] = c + 1
		} else {
			continue
		}
		tamper(t, s, fmt.Sprintf(`UPDATE ops SET payload = '%s' WHERE seq = %d`, strings.ReplaceAll(string(flipped), "'", "''"), seq))
		_, err := s.VerifyChain(ctx)
		var te *TamperError
		if !errors.As(err, &te) || te.Seq != seq {
			t.Fatalf("flipping byte %d of op %d: err = %v", pos, seq, err)
		}
		tamper(t, s, fmt.Sprintf(`UPDATE ops SET payload = '%s' WHERE seq = %d`, strings.ReplaceAll(payload, "'", "''"), seq)) // put it back
		if _, err := s.VerifyChain(ctx); err != nil {
			t.Fatalf("restoring op %d: %v", seq, err)
		}
	}
}

// ---- upgrading an old database ------------------------------------------------------------

// asVersion3 turns a store into what Day 26 left behind: no chain at all.
func asVersion3(t *testing.T, s *Store) string {
	t.Helper()
	path := s.path()
	for _, q := range []string{`DROP TABLE invites`, `DROP TABLE op_sigs`, `DROP TABLE device_keys`, `DROP TABLE node_devices`, `DROP TABLE op_chain`, `DELETE FROM schema_migrations WHERE version >= 4`, `DELETE FROM meta WHERE key = 'chain_retro_ops'`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	return path
}

func TestAnOldDatabaseIsChainedWhenItIsUpgradedAndSaysSo(t *testing.T) {
	ctx := context.Background()
	s, _ := chained(t, "laptop", 4)
	want, _ := s.Heads(ctx)
	path := asVersion3(t, s)

	s2 := open(t, path, Options{})
	rep, err := s2.VerifyChain(ctx)
	if err != nil || rep.Ops != 6 || rep.Retro != 6 {
		t.Fatalf("after the upgrade: %+v err %v", rep, err)
	}
	if got := rep.Heads; len(got) != 1 || got[0] != want[0] {
		t.Errorf("chaining the same ops must give the same heads: %+v vs %+v", got, want)
	}
	// New ops continue the chain and are not counted as retroactive.
	if _, _, err := s2.Create(ctx, task.NewTask{Title: "new", Priority: task.PriorityLow}); err != nil {
		t.Fatal(err)
	}
	s3 := open(t, path, Options{})
	rep, err = s3.VerifyChain(ctx)
	if err != nil || rep.Ops != 7 || rep.Retro != 6 {
		t.Errorf("after reopening: %+v err %v", rep, err)
	}
	if v, _ := schemaVersion(ctx, s3.db); v != 6 {
		t.Errorf("schema version %d", v)
	}
}

func TestAChainThatGoesMissingLaterIsReportedNeverQuietlyRebuilt(t *testing.T) {
	ctx := context.Background()
	s, _ := chained(t, "laptop", 4)
	tamper(t, s, `DELETE FROM op_chain WHERE seq >= 3`)
	path := s.path()
	s.Close()

	s2 := open(t, path, Options{})
	var n int
	s2.db.QueryRow(`SELECT COUNT(*) FROM op_chain`).Scan(&n)
	if n != 2 {
		t.Fatalf("reopening re-chained the log (%d entries): that would hide the deletion", n)
	}
	wantTamper(t, func() error { _, err := s2.VerifyChain(ctx); return err }(), "laptop", 3, "no entry")
	// And a write on top of a damaged chain is refused rather than extending it.
	if _, _, err := s2.Create(ctx, task.NewTask{Title: "more", Priority: task.PriorityLow}); err == nil {
		t.Error("a write was accepted on a chain with a hole in it")
	}
}

// ---- sync ------------------------------------------------------------------------------------

func TestTheSameOpHasTheSameHashOnEveryReplica(t *testing.T) {
	ctx := context.Background()
	a, ca := replicaAt(t, "laptop", epoch)
	b, cb := replicaAt(t, "phone", epoch.Add(time.Second))
	mk(t, a, "from the laptop")
	ca.tick(time.Minute)
	mk(t, b, "from the phone")
	cb.tick(time.Minute)
	pull(t, a, b, 1)
	pull(t, b, a, 1)
	ha, _ := a.Heads(ctx)
	hb, _ := b.Heads(ctx)
	if fmt.Sprint(ha) != fmt.Sprint(hb) || len(ha) != 2 {
		t.Errorf("heads differ: %v vs %v", ha, hb)
	}
	for _, s := range []*Store{a, b} {
		if _, err := s.Verify(ctx); err != nil {
			t.Errorf("%s: %v", s.node, err)
		}
	}
	// Ops read from a replica carry the hash, and it survives a JSON round trip.
	ops, _, _ := a.OpsSince(ctx, Summary{}, 100)
	raw, _ := json.Marshal(ops)
	var back []Op
	json.Unmarshal(raw, &back)
	for i := range ops {
		if ops[i].Hash == "" || back[i].Hash != ops[i].Hash {
			t.Errorf("op %d hash %q -> %q", i, ops[i].Hash, back[i].Hash)
		}
	}
}

func TestAnOpWithTheWrongHashIsRefusedAndNothingIsKept(t *testing.T) {
	ctx := context.Background()
	a, _ := chained(t, "laptop", 3)
	b, _ := replicaAt(t, "phone", epoch)
	ops, _, _ := a.OpsSince(ctx, Summary{}, 100)
	bad := append([]Op(nil), ops...)
	bad[3].Hash = strings.Repeat("d", 64) // op 4's claimed hash is wrong; ops 1-3 are fine
	_, err := b.ApplyRemote(ctx, bad)
	if !errors.Is(err, ErrChain) {
		t.Fatalf("err = %v, want ErrChain", err)
	}
	if n := countOps(t, b); n != 0 {
		t.Errorf("%d ops were kept from a refused batch", n)
	}
	if _, err := b.ApplyRemote(ctx, ops); err != nil {
		t.Fatalf("the honest batch: %v", err)
	}
	if hb, _ := b.Heads(ctx); fmt.Sprint(hb) != fmt.Sprint(mustHeads(t, a)) {
		t.Errorf("heads differ after a clean sync")
	}
}

func mustHeads(t *testing.T, s *Store) []Head {
	t.Helper()
	h, err := s.Heads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestOpsWithoutAHashAreStillAcceptedAndChainedLocally(t *testing.T) {
	ctx := context.Background()
	a, _ := chained(t, "laptop", 2)
	b, _ := replicaAt(t, "phone", epoch)
	ops, _, _ := a.OpsSince(ctx, Summary{}, 100)
	for i := range ops {
		ops[i].Hash = "" // an older peer, or an op built by hand
	}
	if _, err := b.ApplyRemote(ctx, ops); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(mustHeads(t, a)) != fmt.Sprint(mustHeads(t, b)) {
		t.Error("the receiver must compute the same chain itself")
	}
	if _, err := b.VerifyChain(ctx); err != nil {
		t.Error(err)
	}
	// Pretty-printed payloads with the right hash are fine too.
	c, _ := replicaAt(t, "tablet", epoch)
	full, _, _ := a.OpsSince(ctx, Summary{}, 100)
	for i := range full {
		var out bytes.Buffer
		json.Indent(&out, full[i].Payload, "", "    ") // same keys, same order, different whitespace
		full[i].Payload = out.Bytes()
	}
	if _, err := c.ApplyRemote(ctx, full); err != nil {
		t.Errorf("re-indented payloads with the real hashes: %v", err)
	}
}

// The scenario the chain exists for. A's old op was quietly rewritten on one
// replica, chain and all, so that replica verifies cleanly on its own. The
// next time it meets a replica that kept the original history, they disagree
// about everything built on it, and sync says so instead of merging.
func TestTwoReplicasThatDisagreeAboutAnEarlierOpRefuseEachOther(t *testing.T) {
	ctx := context.Background()
	a, ca := replicaAt(t, "laptop", epoch)
	mk(t, a, "the original")
	honest, _ := replicaAt(t, "honest", epoch)
	pull(t, honest, a, 10)
	liar, _ := replicaAt(t, "liar", epoch)
	pull(t, liar, a, 10)

	// On "liar", rewrite laptop's op 1 and recompute its chain.
	tamper(t, liar, `UPDATE ops SET payload = replace(payload, 'the original', 'the forgery') WHERE node_id = 'laptop' AND seq = 1`)
	ops, _ := liar.Ops(ctx, 0, 10)
	h := OpHash(ops[0], GenesisHash)
	if _, err := liar.db.Exec(`UPDATE op_chain SET hash = ? WHERE node_id = 'laptop' AND seq = 1`, h); err != nil {
		t.Fatal(err)
	}
	if _, err := liar.VerifyChain(ctx); err != nil {
		t.Fatalf("the rewritten replica looks fine on its own: %v", err)
	}

	// The laptop goes on working. Its next op's hash is built on the real op 1.
	ca.tick(time.Minute)
	mk(t, a, "after")
	next, _, _ := a.OpsSince(ctx, Summary{"laptop": 1}, 10)
	_, err := liar.ApplyRemote(ctx, next)
	if !errors.Is(err, ErrChain) {
		t.Fatalf("liar accepted an op built on a history it does not have: %v", err)
	}
	if n := countOps(t, liar); n != 1 {
		t.Errorf("the refused batch left %d ops", n)
	}
	// An honest replica takes the same op without complaint.
	if _, err := honest.ApplyRemote(ctx, next); err != nil {
		t.Errorf("honest replica: %v", err)
	}
	// And when the liar's copy of op 1 is offered to the honest one, same content class: refused as a fork.
	op1, _, _ := liar.OpsSince(ctx, Summary{}, 10)
	if _, err := honest.ApplyRemote(ctx, op1); !errors.Is(err, ErrFork) {
		t.Errorf("honest replica given the forged op 1: %v, want ErrFork", err)
	}
}

func TestADuplicateWithTheSameContentButADifferentHashIsRefused(t *testing.T) {
	ctx := context.Background()
	a, _ := chained(t, "laptop", 2)
	b, _ := replicaAt(t, "phone", epoch)
	ops, _, _ := a.OpsSince(ctx, Summary{}, 100)
	if _, err := b.ApplyRemote(ctx, ops); err != nil {
		t.Fatal(err)
	}
	ops[1].Hash = strings.Repeat("e", 64)
	if _, err := b.ApplyRemote(ctx, ops); !errors.Is(err, ErrChain) {
		t.Errorf("err = %v, want ErrChain: the op is the same but it claims a different past", err)
	}
	ops[1].Hash = ""
	if _, err := b.ApplyRemote(ctx, ops); err != nil {
		t.Errorf("a duplicate with no hash is just a duplicate: %v", err)
	}
}

// path is the file a test store lives in.
func (s *Store) path() string {
	rows, err := s.db.Query(`PRAGMA database_list`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			panic(err)
		}
		if name == "main" {
			return file
		}
	}
	panic("no main database")
}
