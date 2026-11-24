package sqlstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"offgrid/internal/blobstore"
)

func age(t *testing.T, s *Store, hash string, by time.Duration) {
	t.Helper()
	p := filepath.Join(s.blobDir, hash[:2], hash[2:])
	old := time.Now().Add(-by)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

// orphan stores a blob that no op references (a crash between the bytes and the op, or a peer's leftover).
func orphan(t *testing.T, s *Store, data []byte) string {
	t.Helper()
	bs, err := s.Blobs()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := bs.Put(context.Background(), bytes.NewReader(data), -1)
	if err != nil {
		t.Fatal(err)
	}
	return ref.Hash
}

func TestMissingBlobsListsWhatTheLogNamesAndTheDiskLacks(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	tk := mk(t, a, "pump")
	p1, p2 := jpeg(5000, 1), jpeg(7000, 2)
	attach(t, a, tk.ID, p1, "image/jpeg", "")
	attach(t, a, tk.ID, p2, "image/jpeg", "")
	if m, err := a.MissingBlobs(ctx, 100); err != nil || len(m) != 0 {
		t.Fatalf("the author has its own bytes: %v %v", err, m)
	}
	pull(t, b, a, 100)
	m, err := b.MissingBlobs(ctx, 100)
	if err != nil || len(m) != 2 {
		t.Fatalf("%v %v", err, m)
	}
	if got, _ := b.MissingBlobs(ctx, 1); len(got) != 1 {
		t.Fatalf("limit ignored: %v", got)
	}
	bs, _ := b.Blobs()
	if _, err := bs.PutExpected(ctx, bytes.NewReader(p1), sum(p1), int64(len(p1))); err != nil {
		t.Fatal(err)
	}
	m, _ = b.MissingBlobs(ctx, 100)
	if len(m) != 1 || m[0].Hash != sum(p2) || m[0].Size != int64(len(p2)) {
		t.Fatalf("%v", m)
	}
}

func TestBlobWantedOnlyForWhatAnOpNamesWithItsSize(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "pump")
	p := jpeg(4000, 1)
	attach(t, s, tk.ID, p, "image/jpeg", "")
	for _, c := range []struct {
		h    string
		size int64
		want bool
	}{{sum(p), 4000, true}, {sum(p), 4001, false}, {sum([]byte("other")), 4000, false}} {
		if got, err := s.BlobWanted(ctx, c.h, c.size); err != nil || got != c.want {
			t.Errorf("%v: got %v %v", c, got, err)
		}
	}
}

func TestCollectionRemovesOnlyOldUnreferencedBlobs(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "pump")
	kept := attach(t, s, tk.ID, jpeg(3000, 1), "image/jpeg", "")
	age(t, s, kept.Hash, 90*24*time.Hour) // old but referenced
	oldOrphan := orphan(t, s, []byte("an orphan from a crash long ago"))
	age(t, s, oldOrphan, 30*24*time.Hour)
	youngOrphan := orphan(t, s, []byte("an orphan from a minute ago"))

	dry, err := s.CollectBlobs(ctx, 7*24*time.Hour, true)
	if err != nil || dry.Removed != 1 || !dry.DryRun {
		t.Fatalf("%v %+v", err, dry)
	}
	bs, _ := s.Blobs()
	if !bs.Has(oldOrphan) {
		t.Fatal("a dry run removed something")
	}
	res, err := s.CollectBlobs(ctx, 7*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 || res.Freed != int64(len("an orphan from a crash long ago")) || res.Young != 1 || res.Referenced != 1 {
		t.Fatalf("%+v", res)
	}
	if bs.Has(oldOrphan) || !bs.Has(youngOrphan) || !bs.Has(kept.Hash) {
		t.Fatal("wrong blobs removed")
	}
	mustVerify(t, s)
}

func TestReAddingAnOldOrphanProtectsItFromCollection(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "pump")
	photo := jpeg(3000, 5)
	h := orphan(t, s, photo)
	age(t, s, h, 60*24*time.Hour)
	// the user attaches the same photo: the blob already exists, so it is not rewritten ...
	a := attach(t, s, tk.ID, photo, "image/jpeg", "")
	// ... and a collection running in the gap before the op was written must not have had it
	if _, err := s.CollectBlobs(ctx, 7*24*time.Hour, false); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Attachments(ctx, tk.ID)
	if len(got) != 1 || !got[0].Present || a.Hash != h {
		t.Fatalf("%+v", got)
	}
	// directly: the age was refreshed by the re-add
	h2 := orphan(t, s, []byte("second"))
	age(t, s, h2, 60*24*time.Hour)
	orphan(t, s, []byte("second")) // stored again
	res, _ := s.CollectBlobs(ctx, 7*24*time.Hour, false)
	if res.Removed != 0 {
		t.Fatalf("a blob stored again a moment ago was collected: %+v", res)
	}
}

func TestACollectionNeverRemovesWhatALogOpNames(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	tk := mk(t, a, "pump")
	photo := jpeg(3000, 8)
	att := attach(t, a, tk.ID, photo, "image/jpeg", "")
	// b fetched the blob first (the way sync does) and has the op too
	pull(t, b, a, 100)
	bs, _ := b.Blobs()
	bs.PutExpected(ctx, bytes.NewReader(photo), att.Hash, att.Size)
	age(t, b, att.Hash, 365*24*time.Hour)
	if err := b.DeleteTask(ctx, tk.ID); err != nil { // the task is gone; the op is still in the log
		t.Fatal(err)
	}
	res, err := b.CollectBlobs(ctx, 7*24*time.Hour, false)
	if err != nil || res.Removed != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if !bs.Has(att.Hash) {
		t.Fatal("a blob named by an op in the log was deleted")
	}
}

func TestTheGracePeriodHasAFloor(t *testing.T) {
	s, _ := freshStore(t)
	for _, g := range []time.Duration{0, time.Second, MinBlobGrace - time.Nanosecond} {
		if _, err := s.CollectBlobs(context.Background(), g, false); !errors.Is(err, ErrGraceTooShort) {
			t.Errorf("grace %s: %v", g, err)
		}
	}
	if _, err := s.CollectBlobs(context.Background(), MinBlobGrace, false); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionOnAStoreWithoutBlobsIsANoOp(t *testing.T) {
	s, err := Open(context.Background(), ":memory:", Options{NodeID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := s.CollectBlobs(context.Background(), 24*time.Hour, false)
	if err != nil || res.Removed != 0 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestBlobStatsCountReferencedMissingAndOrphans(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	tk := mk(t, a, "pump")
	attach(t, a, tk.ID, jpeg(1000, 1), "image/jpeg", "")
	attach(t, a, tk.ID, jpeg(2000, 2), "image/jpeg", "")
	orphan(t, a, []byte("xyz"))
	st, err := a.BlobStats(ctx)
	if err != nil || st.Blobs != 3 || st.Bytes != 3003 || st.Referenced != 2 || st.Missing != 0 || st.Orphans != 1 || st.OrphanBytes != 3 {
		t.Fatalf("%v %+v", err, st)
	}
	pull(t, b, a, 100)
	st, _ = b.BlobStats(ctx)
	if st.Blobs != 0 || st.Referenced != 2 || st.Missing != 2 || st.MissingBytes != 3000 {
		t.Fatalf("%+v", st)
	}
}

func TestAFullDiskRefusesAnAttachmentWithoutRecordingIt(t *testing.T) {
	ctx := context.Background()
	s := open(t, newDB(t), Options{NodeID: "n", BlobReserveBytes: 1 << 20})
	tk := mk(t, s, "pump")
	bs, _ := s.Blobs()
	bs.SetFreeFunc(func(string) (uint64, error) { return 1 << 20, nil }) // exactly the reserve: nothing fits
	before := opCount(t, s)
	_, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(jpeg(5000, 1)), 5000, "image/jpeg", "")
	if !errors.Is(err, blobstore.ErrNoSpace) {
		t.Fatalf("got %v", err)
	}
	var ns *blobstore.NoSpaceError
	if !errors.As(err, &ns) || ns.Need != 5000 {
		t.Fatalf("does not say why: %v", err)
	}
	if opCount(t, s) != before {
		t.Fatal("an op was recorded for a file that was not stored")
	}
}
