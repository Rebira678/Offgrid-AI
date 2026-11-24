package sqlstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"offgrid/internal/blobstore"
	"offgrid/internal/task"
)

// Attachments.
//
// A photo of the fault decides whether a work order is taken seriously, so a task can carry files. The
// bytes do not go in the op log (it must stay small enough to sync and verify forever): they go in a
// content-addressed blob store next to the database, and the log carries an attachment.added op with the
// file's SHA-256, size and media type. Consequences worth knowing:
//
//   - The op can arrive before the bytes. A replica that holds the op and not the blob shows the
//     attachment as "not here yet" (Attachment.Present false); fetching blobs is Day 48.
//   - Applying the op never touches the disk, so a rebuild or a replay does not depend on the blob store.
//   - The hash in the op is signed with the rest of the op, so a peer cannot swap the bytes: anything
//     whose SHA-256 differs is refused on receipt (blobstore.PutExpected).
//   - Disk growth is the size of the blob directory, not the size of the log. Nothing is deleted yet
//     (Day 48 adds garbage collection with a grace period).
//   - There is no "remove attachment" op.

// ErrNoBlobStore is returned when attachments are used on a store that has nowhere to keep files
// (a ":memory:" database with no Options.BlobDir).
var ErrNoBlobStore = errors.New("sqlstore: this store has no place to keep attachments")

// ErrBadAttachment means the file is not an acceptable attachment (type, name, or content that does not
// look like its declared type). Errors wrap it so callers can tell it from a disk failure.
var ErrBadAttachment = errors.New("sqlstore: not an acceptable attachment")

const (
	// MaxAttachmentBytes is the largest size an attachment.added op may declare, whatever any one
	// replica's cap is. It stops an op from describing an absurd file; the cap actually enforced when
	// bytes arrive is Options.MaxBlobBytes.
	MaxAttachmentBytes = 256 << 20
	// MaxAttachmentsPerTask limits what this replica will add to one task. It is not enforced on ops
	// received from others: the fold must give every replica the same view.
	MaxAttachmentsPerTask = 20
	maxAttachmentName     = 120
)

// attachmentTypes are the media types an attachment may declare. No SVG, HTML or script type: a stored
// file may be served back to a browser, and active content from a field phone is not something to host.
var attachmentTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true,
	"image/heic": true, "image/heif": true,
	"application/pdf": true, "text/plain": true, "video/mp4": true,
}

type attachmentPayload struct {
	Task string `json:"task"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// checkAttachment is the rule for an attachment.added payload, used when writing and when applying.
func checkAttachment(p attachmentPayload) error {
	switch {
	case p.Task == "":
		return errors.New("an attachment needs a task")
	case !blobstore.ValidHash(p.Hash):
		return errors.New("an attachment's hash must be 64 lowercase hex characters")
	case p.Size < 1 || p.Size > MaxAttachmentBytes:
		return fmt.Errorf("an attachment's size must be between 1 and %d bytes, not %d", MaxAttachmentBytes, p.Size)
	case !attachmentTypes[p.Type]:
		return fmt.Errorf("attachment type %q is not one this version stores", p.Type)
	}
	return checkAttachmentName(p.Name)
}

func checkAttachmentName(n string) error {
	if !utf8.ValidString(n) || utf8.RuneCountInString(n) > maxAttachmentName {
		return fmt.Errorf("an attachment's name must be valid text of at most %d characters", maxAttachmentName)
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return errors.New("an attachment's name may not contain control characters or path separators")
		}
	}
	return nil
}

// Attachment is one file attached to a task, as the view holds it.
type Attachment struct {
	Task    string    `json:"task"`
	Hash    string    `json:"hash"`
	Size    int64     `json:"size"`
	Type    string    `json:"type"`
	Name    string    `json:"name"`
	At      time.Time `json:"at"`
	Node    string    `json:"node"` // the replica that added it
	Seq     int64     `json:"seq"`
	Present bool      `json:"present"` // the bytes are in this replica's blob store
}

func taskRowExists(ctx context.Context, x execer, id string) (bool, error) {
	var n int
	if err := x.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id = ?`, id).Scan(&n); err != nil {
		return false, err
	}
	return n == 1, nil
}

// blobStore opens the blob directory on first use.
func (s *Store) blobStore() (*blobstore.Store, error) {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	if s.blobs != nil {
		return s.blobs, nil
	}
	if s.blobDir == "" {
		return nil, ErrNoBlobStore
	}
	b, err := blobstore.New(s.blobDir, s.blobMax)
	if err != nil {
		return nil, err
	}
	if s.blobReserve > 0 {
		b.SetReserve(s.blobReserve)
	}
	s.blobs = b
	return b, nil
}

// Blobs returns the blob store (for sync and for serving files), or ErrNoBlobStore.
func (s *Store) Blobs() (*blobstore.Store, error) { return s.blobStore() }

// sniffOK checks that the first bytes of a file look like the type it claims. It is a sanity check
// (a renamed executable is not a JPEG), not a security boundary: the file is only ever served as the
// type the op declares, with sniffing disabled.
func sniffOK(mediaType string, head []byte) bool {
	switch mediaType {
	case "image/heic", "image/heif":
		return len(head) >= 12 && string(head[4:8]) == "ftyp"
	case "text/plain":
		return !bytes.Contains(head, []byte{0}) && utf8Prefix(head)
	}
	got := http.DetectContentType(head)
	if i := strings.IndexByte(got, ';'); i >= 0 {
		got = got[:i]
	}
	return got == mediaType
}

// utf8Prefix reports whether b is valid UTF-8, allowing a character cut by the end of the sample.
func utf8Prefix(b []byte) bool {
	for i := 0; i <= 3 && i < len(b); i++ {
		if utf8.Valid(b[:len(b)-i]) {
			return true
		}
	}
	return false
}

// AddAttachment stores the bytes of r and attaches them to a task. declared is the size the caller
// knows (a file's length, an upload's Content-Length) or -1. The bytes are streamed to disk and never
// held in memory; a file over the cap is refused (before any byte is read when the size is declared).
// The blob is stored first and the op written second, so an acknowledged attachment always has its
// bytes; a crash between the two leaves an unreferenced blob, which is harmless (and collected on Day 48).
func (s *Store) AddAttachment(ctx context.Context, taskID string, r io.Reader, declared int64, mediaType, name string) (Attachment, error) {
	if err := ctx.Err(); err != nil {
		return Attachment{}, err
	}
	mt, _, err := mime.ParseMediaType(mediaType)
	if err != nil || !attachmentTypes[mt] {
		return Attachment{}, fmt.Errorf("%w: type %q is not one this version stores", ErrBadAttachment, mediaType)
	}
	name = strings.TrimSpace(name)
	if err := checkAttachmentName(name); err != nil {
		return Attachment{}, fmt.Errorf("%w: %v", ErrBadAttachment, err)
	}
	bs, err := s.blobStore()
	if err != nil {
		return Attachment{}, err
	}
	if declared > bs.MaxBytes() { // before looking at a single byte
		return Attachment{}, fmt.Errorf("%w: %d bytes declared, the cap is %d", blobstore.ErrTooLarge, declared, bs.MaxBytes())
	}
	if _, err := s.Get(ctx, taskID); err != nil { // fail early, before the bytes are written
		return Attachment{}, err
	}
	br := bufio.NewReaderSize(r, 512)
	head, perr := br.Peek(512)
	if perr != nil && perr != io.EOF && !errors.Is(perr, bufio.ErrBufferFull) {
		return Attachment{}, perr
	}
	if len(head) == 0 {
		return Attachment{}, blobstore.ErrEmpty
	}
	if !sniffOK(mt, head) {
		return Attachment{}, fmt.Errorf("%w: the file does not look like %s", ErrBadAttachment, mt)
	}
	ref, err := bs.Put(ctx, br, declared)
	if err != nil {
		return Attachment{}, err
	}
	var out Attachment
	err = s.write(ctx, func(tx execer) error {
		if _, err := getTask(ctx, tx, taskID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE task_id = ?`, taskID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxAttachmentsPerTask {
			return fmt.Errorf("%w: a task holds at most %d attachments", task.ErrInvalid, MaxAttachmentsPerTask)
		}
		p := attachmentPayload{Task: taskID, Hash: ref.Hash, Size: ref.Size, Type: mt, Name: name}
		if err := checkAttachment(p); err != nil {
			return fmt.Errorf("%w: %v", ErrBadAttachment, err)
		}
		if err := s.record(ctx, tx, KindAttachmentAdded, p); err != nil {
			return err
		}
		list, err := listAttachments(ctx, tx, taskID)
		if err != nil {
			return err
		}
		out = list[len(list)-1]
		return nil
	})
	if err != nil {
		return Attachment{}, err
	}
	out.Present = true
	return out, nil
}

// Attachments lists a task's attachments, oldest first, saying which have their bytes here.
func (s *Store) Attachments(ctx context.Context, taskID string) ([]Attachment, error) {
	list, err := listAttachments(ctx, s.read(), taskID)
	if err != nil {
		return nil, err
	}
	s.markPresent(list)
	return list, nil
}

// AttachmentsByTask lists every task's attachments in one query (for the dashboard), oldest first.
func (s *Store) AttachmentsByTask(ctx context.Context) (map[string][]Attachment, error) {
	list, err := canonAttachments(ctx, s.read())
	if err != nil {
		return nil, err
	}
	s.markPresent(list)
	out := map[string][]Attachment{}
	for _, a := range list {
		out[a.Task] = append(out[a.Task], a)
	}
	return out, nil
}

func (s *Store) markPresent(list []Attachment) {
	bs, err := s.blobStore()
	if err != nil {
		return
	}
	for i := range list {
		list[i].Present = bs.Has(list[i].Hash)
	}
}

// OpenAttachment opens an attachment's bytes for reading, after checking that the task has it. The
// caller closes the file and serves it as the returned Attachment.Type.
func (s *Store) OpenAttachment(ctx context.Context, taskID, hash string) (*os.File, Attachment, error) {
	list, err := s.Attachments(ctx, taskID)
	if err != nil {
		return nil, Attachment{}, err
	}
	for _, a := range list {
		if a.Hash == hash {
			bs, err := s.blobStore()
			if err != nil {
				return nil, a, err
			}
			f, err := bs.Open(hash)
			return f, a, err
		}
	}
	return nil, Attachment{}, task.ErrNotFound
}

func listAttachments(ctx context.Context, x execer, taskID string) ([]Attachment, error) {
	rows, err := x.QueryContext(ctx, `SELECT task_id, hash, size, type, name, at, node, seq FROM attachments WHERE task_id = ? ORDER BY at, node, seq`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAttachments(rows)
}

func scanAttachments(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Attachment, error) {
	var out []Attachment
	for rows.Next() {
		var a Attachment
		var at int64
		if err := rows.Scan(&a.Task, &a.Hash, &a.Size, &a.Type, &a.Name, &at, &a.Node, &a.Seq); err != nil {
			return nil, err
		}
		a.At = fromNanos(at)
		out = append(out, a)
	}
	return out, rows.Err()
}

// canonAttachments is every attachment row in a fixed order, for the view's fingerprint.
func canonAttachments(ctx context.Context, x execer) ([]Attachment, error) {
	rows, err := x.QueryContext(ctx, `SELECT task_id, hash, size, type, name, at, node, seq FROM attachments ORDER BY at, node, seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAttachments(rows)
}
