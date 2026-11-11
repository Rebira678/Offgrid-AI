package sqlstore

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/task"
)

// TestDay30GoldenDatabaseMigrationToSignedOps tests that a legacy database
// from Day 30 (Schema 4 with op_chain, but no op_sigs or genesis_attestations)
// smoothly upgrades to Schema 5, backfills genesis attestations for each replica's
// head, verifies pre-migration ops as "trusted as of migration", and enforces
// signed ops for all subsequent writes.
func TestDay30GoldenDatabaseMigrationToSignedOps(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy_day30.db")

	// 1. Manually construct a Schema 4 database matching Day 30.
	raw, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}

	ddl := []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`INSERT INTO schema_migrations (version, name) VALUES (1, 'm1'), (2, 'm2'), (3, 'm3'), (4, 'm4')`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID`,
		`INSERT INTO meta (key, value) VALUES ('node_id', 'node-legacy-a'), ('created_at', '2026-10-30T00:00:00Z')`,
		`CREATE TABLE ops (
			pos     INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id TEXT    NOT NULL,
			seq     INTEGER NOT NULL CHECK (seq >= 1),
			ts      INTEGER NOT NULL,
			kind    TEXT    NOT NULL,
			payload TEXT    NOT NULL CHECK (json_valid(payload)),
			counter INTEGER NOT NULL DEFAULT 0,
			UNIQUE (node_id, seq)
		)`,
		`CREATE TABLE op_chain (
			node_id TEXT NOT NULL,
			seq     INTEGER NOT NULL,
			prev    TEXT NOT NULL,
			hash    TEXT NOT NULL,
			PRIMARY KEY (node_id, seq)
		)`,
		`CREATE TABLE tasks (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			site TEXT NOT NULL,
			priority TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			completed_at INTEGER
		)`,
		`CREATE TABLE notes (
			task_id TEXT NOT NULL,
			num INTEGER NOT NULL,
			text TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (task_id, num)
		)`,
	}
	for _, stmt := range ddl {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("setup DDL failed: %v: %s", err, stmt)
		}
	}

	// Insert legacy ops for two replicas: node-legacy-a (3 ops) and node-legacy-b (2 ops).
	type legacyOp struct {
		node    string
		seq     int64
		kind    string
		payload createdPayload
		ts      int64
	}
	legacyOps := []legacyOp{
		{"node-legacy-a", 1, KindTaskCreated, createdPayload{ID: "T1", Title: "Legacy task 1", Priority: task.PriorityMedium}, 1700000000000000000},
		{"node-legacy-a", 2, KindTaskCreated, createdPayload{ID: "T2", Title: "Legacy task 2", Priority: task.PriorityLow}, 1700000001000000000},
		{"node-legacy-a", 3, KindTaskCreated, createdPayload{ID: "T3", Title: "Legacy task 3", Priority: task.PriorityHigh}, 1700000002000000000},
		{"node-legacy-b", 1, KindTaskCreated, createdPayload{ID: "T4", Title: "Legacy task 4", Priority: task.PriorityMedium}, 1700000000500000000},
		{"node-legacy-b", 2, KindTaskCreated, createdPayload{ID: "T5", Title: "Legacy task 5", Priority: task.PriorityLow}, 1700000001500000000},
	}

	prevs := make(map[string]string)
	for _, lo := range legacyOps {
		prev := prevs[lo.node]
		if prev == "" {
			prev = GenesisHash
		}
		payBytes, _ := json.Marshal(lo.payload)
		op := Op{
			NodeID:  lo.node,
			Seq:     lo.seq,
			TS:      time.Unix(0, lo.ts).UTC(),
			Counter: 0,
			Kind:    lo.kind,
			Payload: payBytes,
		}
		hash := OpHash(op, prev)
		prevs[lo.node] = hash

		_, err := raw.Exec(`INSERT INTO ops (node_id, seq, ts, kind, payload, counter) VALUES (?, ?, ?, ?, ?, ?)`,
			lo.node, lo.seq, lo.ts, lo.kind, string(payBytes), 0)
		if err != nil {
			t.Fatalf("insert legacy op failed: %v", err)
		}
		_, err = raw.Exec(`INSERT INTO op_chain (node_id, seq, prev, hash) VALUES (?, ?, ?, ?)`,
			lo.node, lo.seq, prev, hash)
		if err != nil {
			t.Fatalf("insert legacy chain failed: %v", err)
		}
	}
	raw.Close()

	// 2. Open store using modern Open (triggering migration 5).
	keyPath := filepath.Join(dir, "device.key")
	id, _, err := identity.LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate identity failed: %v", err)
	}

	st, err := Open(ctx, dbPath, Options{
		Identity:         id,
		RequireSignedOps: true,
		MustExist:        true,
	})
	if err != nil {
		t.Fatalf("Open failed during migration: %v", err)
	}
	defer st.Close()

	// 3. Verify that migration 5 backfilled genesis_attestations for both legacy replicas.
	report, err := st.Verify(ctx)
	if err != nil {
		t.Fatalf("st.Verify failed: %v", err)
	}

	if report.SignedOps != 0 {
		t.Errorf("expected 0 signed ops before new writes, got %d", report.SignedOps)
	}
	if report.AttestedOps != 5 {
		t.Errorf("expected 5 attested ops, got %d", report.AttestedOps)
	}
	if report.Attestations != 2 {
		t.Errorf("expected 2 genesis attestations (one per replica), got %d", report.Attestations)
	}

	// 4. Create a new task post-migration on node-legacy-a (seq 4).
	created, _, err := st.Create(ctx, task.NewTask{
		Title:    "Post migration task",
		Priority: task.PriorityHigh,
	})
	if err != nil {
		t.Fatalf("st.Create failed post-migration: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected non-empty created task ID")
	}

	// 5. Verify again: now 1 signed op and 5 attested ops!
	report, err = st.Verify(ctx)
	if err != nil {
		t.Fatalf("st.Verify failed after create: %v", err)
	}
	if report.SignedOps != 1 {
		t.Errorf("expected 1 signed op, got %d", report.SignedOps)
	}
	if report.AttestedOps != 5 {
		t.Errorf("expected 5 attested ops, got %d", report.AttestedOps)
	}

	// 6. Test Append-Only Triggers on op_sigs and genesis_attestations.
	raw, err = OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer raw.Close()

	// Update op_sigs must fail
	_, err = raw.Exec(`UPDATE op_sigs SET sig = X'1234' WHERE seq = 4`)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only abort on UPDATE op_sigs, got %v", err)
	}

	// Delete from op_sigs must fail
	_, err = raw.Exec(`DELETE FROM op_sigs WHERE seq = 4`)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only abort on DELETE FROM op_sigs, got %v", err)
	}

	// Update genesis_attestations must fail
	_, err = raw.Exec(`UPDATE genesis_attestations SET hash = 'tampered' WHERE node_id = 'node-legacy-a'`)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only abort on UPDATE genesis_attestations, got %v", err)
	}

	// Delete from genesis_attestations must fail
	_, err = raw.Exec(`DELETE FROM genesis_attestations WHERE node_id = 'node-legacy-a'`)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only abort on DELETE FROM genesis_attestations, got %v", err)
	}
}

// TestDay30TamperedGenesisAttestationRejection tests that tampering with
// a backfilled genesis attestation (signature or head hash) is caught by Verify.
func TestDay30TamperedGenesisAttestationRejection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tampered_attest.db")

	raw, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`INSERT INTO schema_migrations (version, name) VALUES (1, 't'), (2, 't'), (3, 't'), (4, 't')`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID`,
		`INSERT INTO meta (key, value) VALUES ('node_id', 'node-tamper')`,
		`CREATE TABLE ops (pos INTEGER PRIMARY KEY AUTOINCREMENT, node_id TEXT, seq INTEGER, ts INTEGER, kind TEXT, payload TEXT, counter INTEGER, UNIQUE(node_id, seq))`,
		`CREATE TABLE op_chain (node_id TEXT, seq INTEGER, prev TEXT, hash TEXT, PRIMARY KEY(node_id, seq))`,
		`CREATE TABLE tasks (id TEXT PRIMARY KEY, title TEXT, site TEXT, priority TEXT, status TEXT, created_at INTEGER, updated_at INTEGER, completed_at INTEGER)`,
		`CREATE TABLE notes (task_id TEXT, num INTEGER, text TEXT, created_at INTEGER, PRIMARY KEY (task_id, num))`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	pay := `{"id":"T1","title":"Original","site":"","priority":"medium"}`
	op := Op{
		NodeID:  "node-tamper",
		Seq:     1,
		TS:      time.Unix(0, 100).UTC(),
		Counter: 0,
		Kind:    KindTaskCreated,
		Payload: []byte(pay),
	}
	h := OpHash(op, GenesisHash)
	raw.Exec(`INSERT INTO ops (node_id, seq, ts, kind, payload, counter) VALUES ('node-tamper', 1, 100, ?, ?, 0)`, KindTaskCreated, pay)
	raw.Exec(`INSERT INTO op_chain VALUES ('node-tamper', 1, ?, ?)`, GenesisHash, h)
	raw.Close()

	id, _ := identity.Generate()
	st, err := Open(ctx, dbPath, Options{Identity: id, MustExist: true})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Drop trigger and tamper with genesis_attestation signature
	raw, _ = OpenDB(dbPath)
	raw.Exec(`DROP TRIGGER IF EXISTS genesis_attestations_append_only_update`)
	corruptSig := make([]byte, 64)
	rand.Read(corruptSig)
	if _, err := raw.Exec(`UPDATE genesis_attestations SET sig = ? WHERE node_id = 'node-tamper'`, corruptSig); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	st2, err := Open(ctx, dbPath, Options{Identity: id, MustExist: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	_, err = st2.Verify(ctx)
	if err == nil {
		t.Fatal("expected Verify to fail on corrupted genesis attestation signature, but passed")
	}
	if !strings.Contains(err.Error(), "genesis attestation") && !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestDay30TamperedLegacyOpCaughtByAttestation verifies that if an attacker
// edits a legacy op (seq 1) and recomputes the chain, the genesis attestation
// created at migration time catches the mismatch.
func TestDay30TamperedLegacyOpCaughtByAttestation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy_tampered.db")

	raw, _ := OpenDB(dbPath)
	for _, stmt := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`INSERT INTO schema_migrations (version, name) VALUES (1, 't'), (2, 't'), (3, 't'), (4, 't')`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID`,
		`INSERT INTO meta (key, value) VALUES ('node_id', 'node-legacy')`,
		`CREATE TABLE ops (pos INTEGER PRIMARY KEY AUTOINCREMENT, node_id TEXT, seq INTEGER, ts INTEGER, kind TEXT, payload TEXT, counter INTEGER, UNIQUE(node_id, seq))`,
		`CREATE TABLE op_chain (node_id TEXT, seq INTEGER, prev TEXT, hash TEXT, PRIMARY KEY(node_id, seq))`,
		`CREATE TABLE tasks (id TEXT PRIMARY KEY, title TEXT, site TEXT, priority TEXT, status TEXT, created_at INTEGER, updated_at INTEGER, completed_at INTEGER)`,
		`CREATE TABLE notes (task_id TEXT, num INTEGER, text TEXT, created_at INTEGER, PRIMARY KEY (task_id, num))`,
	} {
		raw.Exec(stmt)
	}

	pay := `{"id":"T1","title":"Legitimate work","site":"","priority":"medium"}`
	op := Op{
		NodeID:  "node-legacy",
		Seq:     1,
		TS:      time.Unix(0, 100).UTC(),
		Counter: 0,
		Kind:    KindTaskCreated,
		Payload: []byte(pay),
	}
	h := OpHash(op, GenesisHash)
	raw.Exec(`INSERT INTO ops (node_id, seq, ts, kind, payload, counter) VALUES ('node-legacy', 1, 100, ?, ?, 0)`, KindTaskCreated, pay)
	raw.Exec(`INSERT INTO op_chain VALUES ('node-legacy', 1, ?, ?)`, GenesisHash, h)
	raw.Close()

	id, _ := identity.Generate()
	st, err := Open(ctx, dbPath, Options{Identity: id, MustExist: true})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Attacker rewrites ops and recomputes op_chain
	raw, _ = OpenDB(dbPath)
	raw.Exec(`DROP TRIGGER IF EXISTS ops_append_only_update`)
	raw.Exec(`DROP TRIGGER IF EXISTS op_chain_append_only_update`)
	editPay := `{"id":"T1","title":"Malicious edit","site":"","priority":"medium"}`
	raw.Exec(`UPDATE ops SET payload = ? WHERE seq = 1`, editPay)

	// Recompute hash
	fakeOp := op
	fakeOp.Payload = []byte(editPay)
	fakeHash := OpHash(fakeOp, GenesisHash)
	raw.Exec(`UPDATE op_chain SET hash = ? WHERE seq = 1`, fakeHash)
	raw.Close()

	st2, err := Open(ctx, dbPath, Options{Identity: id, MustExist: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	_, err = st2.Verify(ctx)
	if err == nil {
		t.Fatal("expected Verify to detect rewritten legacy op via genesis attestation, but passed")
	}
	if !strings.Contains(err.Error(), "genesis attestation hash does not match") {
		t.Errorf("unexpected error: %v", err)
	}
}
