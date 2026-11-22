package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/blobstore"
	"offgrid/internal/task"
)

// jpeg returns n bytes that begin like a JPEG, made different by seed.
func jpeg(n int, seed byte) []byte {
	b := bytes.Repeat([]byte{seed}, n)
	copy(b, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0})
	return b
}

func png(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("\x89PNG\r\n\x1a\n"))
	return b
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func attach(t *testing.T, s *Store, id string, data []byte, typ, name string) Attachment {
	t.Helper()
	a, err := s.AddAttachment(context.Background(), id, bytes.NewReader(data), int64(len(data)), typ, name)
	if err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}
	return a
}

func opCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ops`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAPhotoIsAttachedStoredByHashAndReadBack(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "Leaking pump")
	photo := jpeg(300_000, 7)
	a := attach(t, s, tk.ID, photo, "image/jpeg", "pump.jpg")
	if a.Hash != sum(photo) || a.Size != int64(len(photo)) || a.Type != "image/jpeg" || a.Name != "pump.jpg" || !a.Present {
		t.Fatalf("%+v", a)
	}
	list, err := s.Attachments(ctx, tk.ID)
	if err != nil || len(list) != 1 || list[0].Hash != a.Hash || !list[0].Present {
		t.Fatalf("%v %+v", err, list)
	}
	f, got, err := s.OpenAttachment(ctx, tk.ID, a.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if !bytes.Equal(b, photo) || got.Type != "image/jpeg" {
		t.Fatal("bytes or type differ")
	}
	if _, _, err := s.OpenAttachment(ctx, tk.ID, sum([]byte("not attached"))); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("a hash the task does not have: %v", err)
	}
	mustVerify(t, s)
}

func TestTheLogCarriesTheReferenceNeverTheBytes(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "pump")
	before := opCount(t, s)
	attach(t, s, tk.ID, jpeg(2<<20, 1), "image/jpeg", "")
	if opCount(t, s) != before+1 {
		t.Fatal("an attachment is exactly one op")
	}
	ops, err := s.Ops(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := ops[len(ops)-1]
	if last.Kind != KindAttachmentAdded || len(last.Payload) > 400 {
		t.Fatalf("%s payload is %d bytes", last.Kind, len(last.Payload))
	}
	fi, err := os.Stat(s.blobDir)
	if err != nil || !fi.IsDir() {
		t.Fatal("no blob directory")
	}
	var dbBytes int64
	for _, ext := range []string{"", "-wal"} {
		if f, err := os.Stat(s.path() + ext); err == nil {
			dbBytes += f.Size()
		}
	}
	if dbBytes > 1<<20 {
		t.Fatalf("the database grew to %d bytes holding a 2 MiB photo", dbBytes)
	}
}

func TestTheOpArrivesBeforeTheBytesAndTheBytesAreCheckedAgainstTheHash(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	tk := mk(t, a, "pump")
	photo := jpeg(100_000, 3)
	att := attach(t, a, tk.ID, photo, "image/jpeg", "fault.jpg")

	pull(t, b, a, 100)
	got, err := b.Attachments(ctx, tk.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	if got[0].Present {
		t.Fatal("b has the op but not the bytes: Present must be false")
	}
	if got[0].Hash != att.Hash || got[0].Size != att.Size || got[0].Name != "fault.jpg" {
		t.Fatalf("%+v", got[0])
	}
	if hashOf(t, a) != hashOf(t, b) {
		t.Fatal("the replicas disagree while one is missing the file")
	}
	mustVerify(t, b)

	bs, err := b.Blobs()
	if err != nil {
		t.Fatal(err)
	}
	// A peer sends damaged bytes for that hash: refused, nothing kept.
	bad := append([]byte(nil), photo...)
	bad[5000] ^= 1
	if _, err := bs.PutExpected(ctx, bytes.NewReader(bad), att.Hash, att.Size); !errors.Is(err, blobstore.ErrHashMismatch) {
		t.Fatalf("got %v", err)
	}
	if g, _ := b.Attachments(ctx, tk.ID); g[0].Present {
		t.Fatal("a corrupted blob was accepted")
	}
	// The right bytes: accepted, and the same hash on both.
	if _, err := bs.PutExpected(ctx, bytes.NewReader(photo), att.Hash, att.Size); err != nil {
		t.Fatal(err)
	}
	if g, _ := b.Attachments(ctx, tk.ID); !g[0].Present {
		t.Fatal("the intact blob was not accepted")
	}
	if hashOf(t, a) != hashOf(t, b) {
		t.Fatal("state differs after the bytes arrived")
	}
}

func TestReplayDoesNotNeedTheBlobStore(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "pump")
	attach(t, s, tk.ID, jpeg(1000, 1), "image/jpeg", "")
	want := hashOf(t, s)
	if err := os.RemoveAll(s.blobDir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild without the blob directory: %v", err)
	}
	if hashOf(t, s) != want {
		t.Fatal("the rebuilt view differs")
	}
	mustVerify(t, s)
	got, _ := s.Attachments(ctx, tk.ID)
	if len(got) != 1 || got[0].Present {
		t.Fatalf("%+v", got)
	}
}

func TestSizeCapIsEnforcedBeforeReadingAndWhileReading(t *testing.T) {
	s := open(t, newDB(t), Options{NodeID: "n", MaxBlobBytes: 10_000})
	tk := mk(t, s, "pump")
	ctx := context.Background()
	before := opCount(t, s)

	r := &probe{r: bytes.NewReader(jpeg(50_000, 1))}
	_, err := s.AddAttachment(ctx, tk.ID, r, 50_000, "image/jpeg", "")
	if !errors.Is(err, blobstore.ErrTooLarge) {
		t.Fatalf("declared size over the cap: %v", err)
	}
	if r.n > 512 { // only the type sniff may have looked at the head
		t.Fatalf("read %d bytes of an over-cap file", r.n)
	}
	r = &probe{r: bytes.NewReader(jpeg(50_000, 1))}
	if _, err := s.AddAttachment(ctx, tk.ID, r, -1, "image/jpeg", ""); !errors.Is(err, blobstore.ErrTooLarge) {
		t.Fatalf("unknown size over the cap: %v", err)
	}
	if r.n > 10_001+4096+32*1024 {
		t.Fatalf("read %d bytes past a 10,000 byte cap", r.n)
	}
	if opCount(t, s) != before {
		t.Fatal("a refused attachment left an op")
	}
	if n := blobFiles(t, s); n != 0 {
		t.Fatalf("%d blob files left by refused attachments", n)
	}
	// exactly at the cap is fine
	attach(t, s, tk.ID, jpeg(10_000, 2), "image/jpeg", "")
}

type probe struct {
	r io.Reader
	n int
}

func (p *probe) Read(b []byte) (int, error) { n, err := p.r.Read(b); p.n += n; return n, err }

func blobFiles(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	filepath.WalkDir(s.blobDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func TestOnlyKnownTypesThatLookLikeTheirTypeAreAccepted(t *testing.T) {
	s, _ := freshStore(t)
	tk := mk(t, s, "pump")
	ctx := context.Background()
	heic := append([]byte{0, 0, 0, 0x18}, []byte("ftypheic....................")...)
	bad := []struct {
		name, typ string
		data      []byte
	}{
		{"svg is active content", "image/svg+xml", []byte("<svg onload=alert(1)/>")},
		{"html", "text/html", []byte("<script>1</script>")},
		{"no type", "", jpeg(100, 1)},
		{"png called jpeg", "image/jpeg", png(100)},
		{"text called png", "image/png", []byte("just text, hello")},
		{"binary called text", "text/plain", append([]byte("abc"), 0, 1, 2)},
		{"executable called pdf", "application/pdf", []byte("\x7fELF....")},
		{"garbage heic", "image/heic", []byte("not a heic file at all")},
		{"junk type", "image/jpeg; x=" + strings.Repeat("a", 5), []byte("")},
	}
	before := opCount(t, s)
	for _, c := range bad {
		if _, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(c.data), int64(len(c.data)), c.typ, ""); err == nil {
			t.Errorf("%s: accepted", c.name)
		} else if c.name != "junk type" && !errors.Is(err, ErrBadAttachment) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if opCount(t, s) != before || blobFiles(t, s) != 0 {
		t.Fatal("a refused type left something behind")
	}
	for _, c := range []struct {
		typ  string
		data []byte
	}{
		{"image/jpeg", jpeg(100, 1)}, {"image/png", png(100)}, {"image/heic", heic},
		{"application/pdf", []byte("%PDF-1.4\n...")}, {"text/plain; charset=utf-8", []byte("a note, with ünïcode")},
	} {
		attach(t, s, tk.ID, c.data, c.typ, "")
	}
}

func TestNamesCannotCarryPathsOrControlCharacters(t *testing.T) {
	s, _ := freshStore(t)
	tk := mk(t, s, "pump")
	for _, n := range []string{"../../etc/passwd", "a/b.jpg", `a\b.jpg`, "bad\x00.jpg", "line\nbreak.jpg", strings.Repeat("x", 121), string([]byte{0xff, 0xfe})} {
		_, err := s.AddAttachment(context.Background(), tk.ID, bytes.NewReader(jpeg(100, 1)), 100, "image/jpeg", n)
		if !errors.Is(err, ErrBadAttachment) {
			t.Errorf("%q: %v", n, err)
		}
	}
	a := attach(t, s, tk.ID, jpeg(100, 1), "image/jpeg", "  Pump leak №2.jpg ")
	if a.Name != "Pump leak №2.jpg" {
		t.Fatalf("name = %q", a.Name)
	}
}

func TestAttachingToAMissingTaskFailsBeforeAnyByteIsStored(t *testing.T) {
	s, _ := freshStore(t)
	_, err := s.AddAttachment(context.Background(), "T99.node-a", bytes.NewReader(jpeg(100, 1)), 100, "image/jpeg", "")
	if !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if blobFiles(t, s) != 0 {
		t.Fatal("bytes were stored for a task that does not exist")
	}
}

func TestATaskHoldsAtMostTwentyAttachments(t *testing.T) {
	s, _ := freshStore(t)
	tk := mk(t, s, "pump")
	for i := 0; i < MaxAttachmentsPerTask; i++ {
		attach(t, s, tk.ID, jpeg(100+i, byte(i)), "image/jpeg", "")
	}
	_, err := s.AddAttachment(context.Background(), tk.ID, bytes.NewReader(jpeg(999, 99)), 999, "image/jpeg", "")
	if !errors.Is(err, task.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestMemoryStoreNeedsABlobDirectory(t *testing.T) {
	s, err := Open(context.Background(), ":memory:", Options{NodeID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tk := mk(t, s, "pump")
	if _, err := s.AddAttachment(context.Background(), tk.ID, bytes.NewReader(jpeg(100, 1)), 100, "image/jpeg", ""); !errors.Is(err, ErrNoBlobStore) {
		t.Fatalf("got %v", err)
	}
	s2, err := Open(context.Background(), ":memory:", Options{NodeID: "m", BlobDir: filepath.Join(t.TempDir(), "b")})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	tk = mk(t, s2, "pump")
	attach(t, s2, tk.ID, jpeg(100, 1), "image/jpeg", "")
}

func TestABlobWithoutAnOpIsHarmless(t *testing.T) {
	// A crash between storing the bytes and writing the op leaves an unreferenced blob.
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "pump")
	bs, _ := s.Blobs()
	if _, err := bs.Put(ctx, bytes.NewReader(jpeg(500, 9)), -1); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, s)
	if got, _ := s.Attachments(ctx, tk.ID); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestAnAttachmentAddedWhileSomeoneElseDeletesTheTaskKeepsTheTaskAlive(t *testing.T) {
	ctx := context.Background()
	a, aclk := replicaAt(t, "node-a", epoch)
	b, bclk := replicaAt(t, "node-b", epoch)
	tk := mk(t, a, "pump")
	pull(t, b, a, 100)
	bclk.tick(time.Minute)
	if err := b.DeleteTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	aclk.tick(2 * time.Minute)
	att := attach(t, a, tk.ID, jpeg(2000, 4), "image/jpeg", "")
	pull(t, a, b, 100)
	pull(t, b, a, 100)
	for _, s := range []*Store{a, b} {
		if _, err := s.Get(ctx, tk.ID); err != nil {
			t.Fatalf("%s: the task did not survive a concurrent attachment: %v", s.node, err)
		}
		got, _ := s.Attachments(ctx, tk.ID)
		if len(got) != 1 || got[0].Hash != att.Hash {
			t.Fatalf("%s: %+v", s.node, got)
		}
	}
	if hashOf(t, a) != hashOf(t, b) {
		t.Fatal("replicas differ")
	}
}

func TestAnAttachmentAfterTheDeleteWasSeenDoesNotResurrectIt(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	b, _ := replicaAt(t, "node-b", epoch)
	tk := mk(t, a, "pump")
	attach(t, a, tk.ID, jpeg(2000, 4), "image/jpeg", "")
	pull(t, b, a, 100)
	if err := b.DeleteTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	pull(t, a, b, 100)
	for _, s := range []*Store{a, b} {
		if _, err := s.Get(ctx, tk.ID); !errors.Is(err, task.ErrNotFound) {
			t.Fatalf("%s: deleted task visible: %v", s.node, err)
		}
	}
	mustVerify(t, a)
	mustVerify(t, b)
}

func TestMalformedAttachmentOpsFromAPeerAreRefused(t *testing.T) {
	ctx := context.Background()
	a, _ := replicaAt(t, "node-a", epoch)
	tk := mk(t, a, "pump")
	attach(t, a, tk.ID, jpeg(100, 1), "image/jpeg", "")
	ops, _, _ := a.OpsSince(ctx, Summary{}, 100)
	good := ops[len(ops)-1]
	var p map[string]any
	if err := json.Unmarshal(good.Payload, &p); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){
		"short hash":       func(m map[string]any) { m["hash"] = "abc" },
		"uppercase hash":   func(m map[string]any) { m["hash"] = strings.ToUpper(m["hash"].(string)) },
		"path as hash":     func(m map[string]any) { m["hash"] = "../../" + strings.Repeat("a", 58) },
		"zero size":        func(m map[string]any) { m["size"] = 0 },
		"negative size":    func(m map[string]any) { m["size"] = -5 },
		"absurd size":      func(m map[string]any) { m["size"] = MaxAttachmentBytes + 1 },
		"svg":              func(m map[string]any) { m["type"] = "image/svg+xml" },
		"no task":          func(m map[string]any) { m["task"] = "" },
		"traversal name":   func(m map[string]any) { m["name"] = "../x" },
		"unknown field":    func(m map[string]any) { m["extra"] = 1 },
		"task not present": func(m map[string]any) { m["task"] = "T77.node-z" },
	}
	for name, mut := range cases {
		fresh, _ := replicaAt(t, "node-c", epoch)
		q := map[string]any{}
		for k, v := range p {
			q[k] = v
		}
		mut(q)
		raw, _ := json.Marshal(q)
		bad := good
		bad.Payload = raw
		bad.Hash = ""
		send := append(append([]Op{}, ops[:len(ops)-1]...), bad) // the task, then the mutated attachment
		if _, err := fresh.ApplyRemote(ctx, send); err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if s, _ := fresh.Summary(ctx); s[good.NodeID] >= good.Seq {
			t.Errorf("%s: the refused op was kept", name)
		}
	}
}
