package sqlstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/blobstore"
	"offgrid/internal/task"
)

func webp(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("RIFF\x24\x00\x00\x00WEBPVP8 "))
	return b
}

func gif(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\xff\xff\xff\x00\x00\x00!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
	return b
}

func mp4(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"))
	return b
}

func pdf(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("%PDF-1.7\n%âãÏÓ\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n"))
	return b
}

// TestAttachmentsUnicodeAndEmojiFilenames verifies handling of UTF-8 filenames,
// boundary length of 120 runes, and rejection of 121 runes.
func TestAttachmentsUnicodeAndEmojiFilenames(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "Solar Inverter Fault")

	// Multi-byte Unicode and Emoji
	unicodeName := "📸_インバーター_Reparación_2026_🚨.jpg"
	data := jpeg(1024, 42)
	att, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(data), int64(len(data)), "image/jpeg", unicodeName)
	if err != nil {
		t.Fatalf("AddAttachment with Unicode failed: %v", err)
	}
	if att.Name != unicodeName {
		t.Fatalf("name mismatch: got %q, want %q", att.Name, unicodeName)
	}

	// Boundary test: Exactly 120 runes (should succeed)
	name120 := strings.Repeat("日", 120)
	att120, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(jpeg(1024, 43)), 1024, "image/jpeg", name120)
	if err != nil {
		t.Fatalf("120-rune name rejected: %v", err)
	}
	if att120.Name != name120 {
		t.Fatalf("120-rune name mismatch: len=%d", len([]rune(att120.Name)))
	}

	// Boundary test: Exactly 121 runes (should fail)
	name121 := strings.Repeat("日", 121)
	_, err = s.AddAttachment(ctx, tk.ID, bytes.NewReader(jpeg(1024, 44)), 1024, "image/jpeg", name121)
	if !errors.Is(err, ErrBadAttachment) {
		t.Fatalf("expected ErrBadAttachment for 121 runes, got %v", err)
	}

	// Whitespace trimming
	paddedName := "   circuit-breaker.png   \t\n "
	attPadded, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(png(512)), 512, "image/png", paddedName)
	if err != nil {
		t.Fatalf("padded name failed: %v", err)
	}
	if attPadded.Name != "circuit-breaker.png" {
		t.Fatalf("expected trimmed name, got %q", attPadded.Name)
	}
}

// TestAttachmentsMediaTypeSniffMatrix verifies detection and allowlist adherence across formats.
func TestAttachmentsMediaTypeSniffMatrix(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "Inspection Checklist")

	validFormats := []struct {
		desc string
		mime string
		data []byte
	}{
		{"WebP Image", "image/webp", webp(1000)},
		{"GIF Image", "image/gif", gif(500)},
		{"MP4 Video", "video/mp4", mp4(2048)},
		{"PDF Document", "application/pdf", pdf(1024)},
		{"Plain UTF-8 Text", "text/plain", []byte("Field technician report: verified junction box.\nOK.\n")},
	}

	for _, tc := range validFormats {
		t.Run(tc.desc, func(t *testing.T) {
			att, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(tc.data), int64(len(tc.data)), tc.mime, tc.desc)
			if err != nil {
				t.Fatalf("AddAttachment(%s) failed: %v", tc.mime, err)
			}
			if att.Type != tc.mime || !att.Present {
				t.Fatalf("attachment %+v not as expected", att)
			}
		})
	}

	// Test forbidden and malicious content types
	maliciousFormats := []struct {
		desc string
		mime string
		data []byte
	}{
		{"SVG XML", "image/svg+xml", []byte("<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>")},
		{"HTML Document", "text/html", []byte("<!DOCTYPE html><html><body><h1>Phishing</h1></body></html>")},
		{"JavaScript Payload", "application/javascript", []byte("fetch('/api/keys')")},
		{"Shell Script", "application/x-sh", []byte("#!/bin/sh\nrm -rf /")},
		{"Binary in Text", "text/plain", []byte("Some text \x00 with forbidden null bytes")},
		{"Mismatch JPEG as PNG", "image/png", jpeg(500, 1)},
		{"Mismatch PNG as PDF", "application/pdf", png(500)},
	}

	for _, tc := range maliciousFormats {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(tc.data), int64(len(tc.data)), tc.mime, "malicious")
			if !errors.Is(err, ErrBadAttachment) {
				t.Fatalf("expected ErrBadAttachment for %s, got %v", tc.desc, err)
			}
		})
	}
}

// TestAttachmentsMaxLimitPerTaskConcurrency tests that concurrent attempts to add
// attachments to a task never exceed MaxAttachmentsPerTask (20).
func TestAttachmentsMaxLimitPerTaskConcurrency(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "Turbine Maintenance")

	// Pre-fill 10 attachments
	for i := 0; i < 10; i++ {
		attach(t, s, tk.ID, jpeg(500+i, byte(i)), "image/jpeg", fmt.Sprintf("pre-%d.jpg", i))
	}

	// Now launch 20 concurrent attachment attempts. Exactly 10 should succeed, and 10 should fail.
	const attempts = 20
	var wg sync.WaitGroup
	var successCount atomic.Int32
	var limitErrCount atomic.Int32

	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			data := jpeg(600+idx, byte(idx+20))
			_, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(data), int64(len(data)), "image/jpeg", fmt.Sprintf("burst-%d.jpg", idx))
			if err == nil {
				successCount.Add(1)
			} else if errors.Is(err, task.ErrInvalid) {
				limitErrCount.Add(1)
			} else {
				t.Errorf("unexpected error on attempt %d: %v", idx, err)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	totalRecorded, err := s.Attachments(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Attachments: %v", err)
	}

	if len(totalRecorded) != MaxAttachmentsPerTask {
		t.Fatalf("expected exactly %d attachments, got %d", MaxAttachmentsPerTask, len(totalRecorded))
	}
	if successCount.Load() != 10 {
		t.Fatalf("expected 10 successes, got %d", successCount.Load())
	}
	if limitErrCount.Load() != 10 {
		t.Fatalf("expected 10 ErrInvalid rejections, got %d", limitErrCount.Load())
	}
}

// TestAttachmentsRebuildAndFingerprintDeterminism verifies that rebuilding the view
// including attachments is strictly idempotent and produces deterministic state hashes.
func TestAttachmentsRebuildAndFingerprintDeterminism(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "Generator Check")

	// Add attachments, notes, and answers
	attach(t, s, tk.ID, jpeg(1024, 1), "image/jpeg", "gen-1.jpg")
	attach(t, s, tk.ID, png(512), "image/png", "gen-2.png")
	if _, err := s.AddNote(ctx, tk.ID, "Oil level normal"); err != nil {
		t.Fatal(err)
	}

	rep1, err := s.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild 1: %v", err)
	}

	rep2, err := s.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild 2: %v", err)
	}

	if rep1.Hash != rep2.Hash {
		t.Fatalf("non-deterministic rebuild fingerprint: %s vs %s", rep1.Hash, rep2.Hash)
	}

	mustVerify(t, s)

	// Tamper with the attachments projection table directly
	_, err = s.db.Exec(`UPDATE attachments SET size = size + 1`)
	if err != nil {
		t.Fatal(err)
	}

	// Verify must detect the tampering
	_, err = s.Verify(ctx)
	if err == nil {
		t.Fatal("Verify failed to catch tampered attachments table")
	}

	// Rebuild repairs the tampered projection
	repRepaired, err := s.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild repair: %v", err)
	}
	if repRepaired.Hash != rep1.Hash {
		t.Fatalf("fingerprint did not restore after rebuild: got %s, want %s", repRepaired.Hash, rep1.Hash)
	}
	mustVerify(t, s)
}

// TestAttachmentsMultiNodeConvergenceWithMissingBlobs simulates multiple peers syncing
// attachments where one peer lacks the blob bytes, verifying CRDT convergence.
func TestAttachmentsMultiNodeConvergenceWithMissingBlobs(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	c, _ := replicaAt(t, "node-c", epoch)

	tk := mk(t, a, "Substation transformer")
	att1 := attach(t, a, tk.ID, jpeg(1000, 11), "image/jpeg", "sub1.jpg")
	att2 := attach(t, a, tk.ID, png(800), "image/png", "sub2.png")

	// Pull from a to b and c
	pull(t, b, a, 100)
	pull(t, c, a, 100)

	// Check b has ops, but not blobs
	bList, err := b.Attachments(ctx, tk.ID)
	if err != nil || len(bList) != 2 {
		t.Fatalf("b attachments: %v, len=%d", err, len(bList))
	}
	if bList[0].Present || bList[1].Present {
		t.Fatal("b should not have blobs present yet")
	}

	// Replicas a, b, and c must have IDENTICAL state hashes despite b and c lacking blobs
	hashA := hashOf(t, a)
	hashB := hashOf(t, b)
	hashC := hashOf(t, c)
	if hashA != hashB || hashA != hashC {
		t.Fatalf("state hash divergence with missing blobs: a=%s b=%s c=%s", hashA, hashB, hashC)
	}

	// Now peer b fetches blob 1 only
	bBlobs, err := b.Blobs()
	if err != nil {
		t.Fatal(err)
	}
	data1 := jpeg(1000, 11)
	_, err = bBlobs.PutExpected(ctx, bytes.NewReader(data1), att1.Hash, att1.Size)
	if err != nil {
		t.Fatalf("PutExpected blob 1 on b: %v", err)
	}

	bListUpdated, _ := b.Attachments(ctx, tk.ID)
	if !bListUpdated[0].Present {
		t.Fatal("bListUpdated[0] should now be present")
	}
	if bListUpdated[1].Present {
		t.Fatal("bListUpdated[1] should still be absent")
	}

	// State hash must STILL be identical
	if hashOf(t, b) != hashA {
		t.Fatalf("state hash changed after partial blob arrival: b=%s a=%s", hashOf(t, b), hashA)
	}

	// Peer b fetches blob 2
	data2 := png(800)
	_, err = bBlobs.PutExpected(ctx, bytes.NewReader(data2), att2.Hash, att2.Size)
	if err != nil {
		t.Fatalf("PutExpected blob 2 on b: %v", err)
	}

	bListComplete, _ := b.Attachments(ctx, tk.ID)
	if !bListComplete[0].Present || !bListComplete[1].Present {
		t.Fatal("all blobs should now be present on b")
	}
	if hashOf(t, b) != hashA {
		t.Fatalf("final state hash mismatch: b=%s a=%s", hashOf(t, b), hashA)
	}
}

// TestAttachmentsCustomBlobDirAndMaxBlobBytes verifies Options.BlobDir and Options.MaxBlobBytes.
func TestAttachmentsCustomBlobDirAndMaxBlobBytes(t *testing.T) {
	ctx := context.Background()
	customDir := filepath.Join(t.TempDir(), "custom-blobs")
	const maxCap = 4096

	s := open(t, newDB(t), Options{
		NodeID:       "node-custom",
		BlobDir:      customDir,
		MaxBlobBytes: maxCap,
	})

	tk := mk(t, s, "Valve replacement")

	// 1. File under cap succeeds
	dataOK := jpeg(2048, 5)
	att, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(dataOK), int64(len(dataOK)), "image/jpeg", "valve-ok.jpg")
	if err != nil {
		t.Fatalf("file under cap failed: %v", err)
	}

	// Verify file is placed in customDir
	shard := att.Hash[:2]
	expectedPath := filepath.Join(customDir, shard, att.Hash[2:])
	if _, err := filepath.Glob(expectedPath); err != nil {
		t.Fatalf("file not found in customDir: %v", err)
	}

	// 2. File over cap (declared) rejected before reading
	dataOversize := jpeg(maxCap+1, 6)
	_, err = s.AddAttachment(ctx, tk.ID, bytes.NewReader(dataOversize), int64(len(dataOversize)), "image/jpeg", "valve-big.jpg")
	if !errors.Is(err, blobstore.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge for declared size %d > %d, got %v", len(dataOversize), maxCap, err)
	}

	// 3. File over cap (undeclared) rejected while reading
	_, err = s.AddAttachment(ctx, tk.ID, bytes.NewReader(dataOversize), -1, "image/jpeg", "valve-big-stream.jpg")
	if !errors.Is(err, blobstore.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge for stream size %d > %d, got %v", len(dataOversize), maxCap, err)
	}
}

// TestAttachmentsConcurrentReadAndAdd verifies concurrent readers during heavy attachment writes.
func TestAttachmentsConcurrentReadAndAdd(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "Pipeline Sensor Calibration")

	const writers = 5
	const readers = 10
	var wg sync.WaitGroup
	var stop atomic.Bool

	// Spawn readers
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_, _ = s.Attachments(ctx, tk.ID)
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}

	// Spawn writers
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			data := jpeg(300+idx*10, byte(idx))
			_, _ = s.AddAttachment(ctx, tk.ID, bytes.NewReader(data), int64(len(data)), "image/jpeg", fmt.Sprintf("sensor-%d.jpg", idx))
		}(i)
	}

	// Let writers finish
	time.Sleep(50 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	list, err := s.Attachments(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Attachments after concurrency: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("expected attachments to be written")
	}
	mustVerify(t, s)
}

// TestAttachmentsDeletedTaskAddAttempts ensures attempting to attach to an already deleted
// task fails immediately with task.ErrNotFound before writing any blob or op.
func TestAttachmentsDeletedTaskAddAttempts(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "Decommissioned Unit")

	if err := s.DeleteTask(ctx, tk.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	beforeOps := opCount(t, s)
	_, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(jpeg(500, 1)), 500, "image/jpeg", "after-delete.jpg")
	if !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("expected task.ErrNotFound on deleted task, got %v", err)
	}

	if opCount(t, s) != beforeOps {
		t.Fatal("an op was recorded for a deleted task")
	}
}

// TestAttachmentsOpenAttachmentEdgeCases verifies error paths of OpenAttachment.
func TestAttachmentsOpenAttachmentEdgeCases(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "Submersible Pump")

	att := attach(t, s, tk.ID, jpeg(1000, 3), "image/jpeg", "pump.jpg")

	// 1. Valid hash, file exists
	f, gotAtt, err := s.OpenAttachment(ctx, tk.ID, att.Hash)
	if err != nil {
		t.Fatalf("OpenAttachment failed: %v", err)
	}
	f.Close()
	if gotAtt.Hash != att.Hash {
		t.Fatalf("hash mismatch: %+v", gotAtt)
	}

	// 2. Non-existent hash for existing task
	nonExistentHash := sum([]byte("totally unattached content"))
	_, _, err = s.OpenAttachment(ctx, tk.ID, nonExistentHash)
	if !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("expected task.ErrNotFound for unattached hash, got %v", err)
	}

	// 3. Non-existent task
	_, _, err = s.OpenAttachment(ctx, "T999.node-xyz", att.Hash)
	if !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("expected task.ErrNotFound for missing task, got %v", err)
	}

	// 4. File record exists in view, but blob missing on disk
	p := filepath.Join(s.blobDir, att.Hash[:2], att.Hash[2:])
	_ = os.Remove(p)
	_, _, err = s.OpenAttachment(ctx, tk.ID, att.Hash)
	if !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("expected blobstore.ErrNotFound for missing blob file, got %v", err)
	}
}

// TestAttachmentsContextCancellation verifies AddAttachment respects context cancellation.
func TestAttachmentsContextCancellation(t *testing.T) {
	s, _ := freshStore(t)
	tk := mk(t, s, "Generator")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(jpeg(500, 1)), 500, "image/jpeg", "cancel.jpg")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
