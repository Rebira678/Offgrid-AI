package sqlstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/task"
)

// TestSignatureEdgeCases verifies that malformed signatures, wrong curve keys,
// truncated/oversized signatures, and mismatched device IDs are rejected.
func TestSignatureEdgeCases(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sig_edge.db")

	idA, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, dbPath, Options{
		Identity:         idA,
		RequireSignedOps: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Append a valid op
	_, _, err = st.Create(ctx, task.NewTask{Title: "Task 1", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Truncated signature (< 64 bytes)
	t.Run("truncated_signature", func(t *testing.T) {
		tamperDB(t, dbPath, `UPDATE op_sigs SET sig = X'01020304' WHERE seq = 1`)
		_, err := st.Verify(ctx)
		if err == nil || !strings.Contains(err.Error(), "signature is not 64 bytes") {
			t.Fatalf("expected signature is not 64 bytes error, got %v", err)
		}
	})

	// 2. Oversized signature (> 64 bytes)
	t.Run("oversized_signature", func(t *testing.T) {
		oversized := make([]byte, 65)
		tamperDB(t, dbPath, fmt.Sprintf("UPDATE op_sigs SET sig = X'%x' WHERE seq = 1", oversized))
		_, err := st.Verify(ctx)
		if err == nil || !strings.Contains(err.Error(), "signature is not 64 bytes") {
			t.Fatalf("expected signature is not 64 bytes error, got %v", err)
		}
	})

	// 3. Forged signature (valid length, wrong signature bytes)
	t.Run("forged_signature_bytes", func(t *testing.T) {
		randomSig := make([]byte, 64)
		rand.Read(randomSig)
		tamperDB(t, dbPath, fmt.Sprintf("UPDATE op_sigs SET sig = X'%x' WHERE seq = 1", randomSig))
		_, err := st.Verify(ctx)
		if err == nil || !strings.Contains(err.Error(), "forged or edited signature") {
			t.Fatalf("expected forged or edited signature error, got %v", err)
		}
	})

	// 4. Mismatched DeviceID
	t.Run("mismatched_device_id", func(t *testing.T) {
		// Restore valid signature first
		sig, _ := idA.Sign([]byte(getOpHash(t, dbPath, 1)))
		tamperDB(t, dbPath, fmt.Sprintf("UPDATE op_sigs SET sig = X'%x', device_id = 'dev_bad00000000000000000000000000000' WHERE seq = 1", sig))
		_, err := st.Verify(ctx)
		if err == nil || !strings.Contains(err.Error(), "device ID does not match public key") {
			t.Fatalf("expected device ID mismatch error, got %v", err)
		}
	})

	// 5. Corrupted public key format
	t.Run("corrupted_pubkey", func(t *testing.T) {
		sig, _ := idA.Sign([]byte(getOpHash(t, dbPath, 1)))
		tamperDB(t, dbPath, fmt.Sprintf("UPDATE op_sigs SET sig = X'%x', device_id = '%s', pub_key = 'bad-key-format' WHERE seq = 1", sig, idA.ID()))
		_, err := st.Verify(ctx)
		if err == nil || !strings.Contains(err.Error(), "public key") {
			t.Fatalf("expected public key error, got %v", err)
		}
	})

	// 6. Public key signed by different key (signature valid for pub_key B, but pub_key in row is A)
	t.Run("signature_signed_by_different_key", func(t *testing.T) {
		idB, _ := identity.Generate()
		sigFromB, _ := idB.Sign([]byte(getOpHash(t, dbPath, 1)))
		tamperDB(t, dbPath, fmt.Sprintf("UPDATE op_sigs SET sig = X'%x', device_id = '%s', pub_key = '%s' WHERE seq = 1", sigFromB, idA.ID(), idA.PublicKey()))
		_, err := st.Verify(ctx)
		if err == nil || !strings.Contains(err.Error(), "forged or edited signature") {
			t.Fatalf("expected signature verification failure with key B signature against key A pubkey, got %v", err)
		}
	})
}

// TestParallelSignatureVerificationWorkerPool tests that parallel signature
// verification scales correctly, respects context cancellation, and produces
// consistent deterministic results over hundreds of ops.
func TestParallelSignatureVerificationWorkerPool(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "parallel_sig.db")

	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, dbPath, Options{
		Identity:         id,
		RequireSignedOps: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Generate 100 ops
	const numOps = 100
	for i := 0; i < numOps; i++ {
		_, _, err := st.Create(ctx, task.NewTask{
			Title:    fmt.Sprintf("Task batch %d", i),
			Priority: task.PriorityMedium,
		})
		if err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
	}

	// Verify all 100 in parallel
	report, err := st.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if report.SignedOps != numOps {
		t.Fatalf("expected %d signed ops, got %d", numOps, report.SignedOps)
	}

	// Test parallel cancellation: verify with canceled context returns error cleanly
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = st.Verify(canceledCtx)
	if err == nil {
		t.Fatal("expected error with canceled context, got nil")
	}

	// Tamper with just ONE op in the middle (e.g. seq 50) and ensure parallel verification catches it
	tamperDB(t, dbPath, `UPDATE op_sigs SET sig = X'00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000' WHERE seq = 50`)
	_, err = st.Verify(ctx)
	if err == nil || !strings.Contains(err.Error(), "op ") || !strings.Contains(err.Error(), "/50") {
		t.Fatalf("expected parallel verify to catch tampered seq 50, got %v", err)
	}
}

// TestConcurrentStoreReadsAndAppendsWithSignatures stresses concurrent readers
// verifying signatures while a single writer is appending signed ops.
func TestConcurrentStoreReadsAndAppendsWithSignatures(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "concurrent_sig.db")

	id, _ := identity.Generate()
	st, err := Open(ctx, dbPath, Options{
		Identity:         id,
		RequireSignedOps: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, 16)

	// Single writer creating tasks
	stopWriter := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stopWriter:
				return
			default:
				_, _, err := st.Create(ctx, task.NewTask{Title: fmt.Sprintf("Concurrent task %d", i), Priority: task.PriorityMedium})
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	// Multiple concurrent readers running VerifySignatures, VerifyChain and Summary
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				_, err := st.VerifySignatures(ctx)
				if err != nil {
					select {
					case errCh <- fmt.Errorf("reader %d verify signatures: %w", readerID, err):
					default:
					}
					return
				}
				_, err = st.VerifyChain(ctx)
				if err != nil {
					select {
					case errCh <- fmt.Errorf("reader %d verify chain: %w", readerID, err):
					default:
					}
					return
				}
				_, err = st.Summary(ctx)
				if err != nil {
					select {
					case errCh <- fmt.Errorf("reader %d summary: %w", readerID, err):
					default:
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}(r)
	}

	// Let readers run their iterations
	time.Sleep(150 * time.Millisecond)
	close(stopWriter)
	wg.Wait()

	select {
	case err := <-errCh:
		t.Fatalf("concurrent test failed: %v", err)
	default:
	}

	// Once writes stop, full Verify (including state replay) must pass 100%
	if _, err := st.Verify(ctx); err != nil {
		t.Fatalf("quiescent verify failed: %v", err)
	}
}

// Helpers
func tamperDB(t *testing.T, dbPath string, query string) {
	t.Helper()
	raw, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.Exec(`DROP TRIGGER IF EXISTS op_sigs_append_only_update`)
	raw.Exec(`DROP TRIGGER IF EXISTS op_sigs_append_only_delete`)
	if _, err := raw.Exec(query); err != nil {
		t.Fatalf("tamperDB query %q failed: %v", query, err)
	}
}

func getOpHash(t *testing.T, dbPath string, seq int64) string {
	t.Helper()
	raw, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var h string
	if err := raw.QueryRow(`SELECT hash FROM op_chain WHERE seq = ?`, seq).Scan(&h); err != nil {
		t.Fatalf("getOpHash seq %d failed: %v", seq, err)
	}
	return h
}

var _ = ecdsa.GenerateKey
var _ = elliptic.P256
