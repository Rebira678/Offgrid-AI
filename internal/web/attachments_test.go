package web

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func jpegData(n int, seed byte) []byte {
	b := bytes.Repeat([]byte{seed}, n)
	copy(b, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0})
	return b
}

func TestTheStateListsAttachmentsAndWhetherTheirBytesAreHere(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	tk := mk(t, r.st, "pump")
	data := jpegData(3000, 1)
	att, err := r.st.AddAttachment(ctx, tk.ID, bytes.NewReader(data), int64(len(data)), "image/jpeg", "leak.jpg")
	if err != nil {
		t.Fatal(err)
	}
	st := build(t, r.cfg)
	if len(st.Tasks) != 1 || len(st.Tasks[0].Files) != 1 {
		t.Fatalf("%+v", st.Tasks)
	}
	f := st.Tasks[0].Files[0]
	if f.Hash != att.Hash || f.Name != "leak.jpg" || f.Size != 3000 || !f.Present || !f.Image {
		t.Fatalf("%+v", f)
	}
	// a replica that has the op and not the bytes says so
	c := &clk{t0}
	b := newStore(t, "pi", c)
	pull(t, b, r.st)
	st2 := build(t, Config{Store: b, Now: c.now})
	if g := st2.Tasks[0].Files; len(g) != 1 || g[0].Present {
		t.Fatalf("%+v", g)
	}
}

func TestAnAttachmentIsServedWithItsDeclaredTypeAndLockedDown(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	tk := mk(t, r.st, "pump")
	data := jpegData(5000, 2)
	att, _ := r.st.AddAttachment(ctx, tk.ID, bytes.NewReader(data), int64(len(data)), "image/jpeg", "leak.jpg")
	code, h, body := r.do("GET", "/api/tasks/"+tk.ID+"/attachments/"+att.Hash, "", nil)
	if code != 200 || body != string(data) {
		t.Fatalf("%d, %d bytes", code, len(body))
	}
	if h.Get("Content-Type") != "image/jpeg" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "inline") {
		t.Fatalf("%v", h)
	}
	// a range works (a phone resuming a large file)
	code, _, part := r.do("GET", "/api/tasks/"+tk.ID+"/attachments/"+att.Hash, "", map[string]string{"Range": "bytes=10-19"})
	if code != 206 || part != string(data[10:20]) {
		t.Fatalf("%d %q", code, part)
	}
}

func TestNonPicturesAreDownloadsNotPages(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	tk := mk(t, r.st, "pump")
	pdf := []byte("%PDF-1.4 a manual")
	att, err := r.st.AddAttachment(ctx, tk.ID, bytes.NewReader(pdf), int64(len(pdf)), "application/pdf", "manual.pdf")
	if err != nil {
		t.Fatal(err)
	}
	_, h, _ := r.do("GET", "/api/tasks/"+tk.ID+"/attachments/"+att.Hash, "", nil)
	if !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || h.Get("Content-Type") != "application/pdf" {
		t.Fatalf("%v", h)
	}
	if st := build(t, r.cfg); st.Tasks[0].Files[0].Image {
		t.Fatal("a PDF was marked as a picture")
	}
}

func TestAnAttachmentThatIsNotThereYetOrNotTheTasksIsNotFound(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	tk := mk(t, r.st, "pump")
	other := mk(t, r.st, "filter")
	data := jpegData(2000, 3)
	att, _ := r.st.AddAttachment(ctx, tk.ID, bytes.NewReader(data), int64(len(data)), "image/jpeg", "")
	if code, _, _ := r.do("GET", "/api/tasks/"+other.ID+"/attachments/"+att.Hash, "", nil); code != 404 {
		t.Fatalf("another task's hash: %d", code)
	}
	for _, p := range []string{"../../etc/passwd", strings.Repeat("a", 64), "x"} {
		if code, _, body := r.do("GET", "/api/tasks/"+tk.ID+"/attachments/"+p, "", nil); code != 404 || strings.Contains(body, "root:") {
			t.Fatalf("%q: %d", p, code)
		}
	}
	// the op is here, the bytes are not
	c := &clk{t0}
	b := newStore(t, "pi", c)
	pull(t, b, r.st)
	rb := newRig(t, func(cf *Config) { cf.Store = b })
	code, _, body := rb.do("GET", "/api/tasks/"+tk.ID+"/attachments/"+att.Hash, "", nil)
	if code != 404 || !strings.Contains(body, "not reached this device") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestAttachmentsAreReadOnlyOverTheDashboard(t *testing.T) {
	r := newRig(t, nil)
	tk := mk(t, r.st, "pump")
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if code, _, _ := r.do(m, "/api/tasks/"+tk.ID+"/attachments/"+strings.Repeat("a", 64), "", map[string]string{ActionHeader: "1"}); code == 200 || code == 201 {
			t.Fatalf("%s was accepted: %d", m, code)
		}
	}
}
