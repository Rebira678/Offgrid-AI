package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// BlobRef names a blob an op refers to.
type BlobRef struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// MinBlobGrace is the shortest grace period CollectBlobs accepts. The grace period is what keeps a
// collection from deleting a blob whose op is about to be written (or about to arrive from a peer), so it
// must be longer than any such gap; ten minutes is far longer than a write, and real grace periods are days.
const MinBlobGrace = 10 * time.Minute

// ErrGraceTooShort is returned by CollectBlobs for a grace period under MinBlobGrace.
var ErrGraceTooShort = fmt.Errorf("sqlstore: the blob grace period must be at least %s", MinBlobGrace)

// MissingBlobs lists up to limit blobs that attachment ops in the view name and this replica does not hold.
// It is what a sync asks peers for.
func (s *Store) MissingBlobs(ctx context.Context, limit int) ([]BlobRef, error) {
	bs, err := s.blobStore()
	if err != nil {
		if errors.Is(err, ErrNoBlobStore) {
			return nil, nil
		}
		return nil, err
	}
	var out []BlobRef
	after := ""
	for limit <= 0 || len(out) < limit {
		page, err := s.blobPage(ctx, after, 500)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			if !bs.Has(r.Hash) {
				out = append(out, r)
				if limit > 0 && len(out) == limit {
					return out, nil
				}
			}
		}
		after = page[len(page)-1].Hash
	}
	return out, nil
}

// HeldBlobs pages through the blobs that ops name and this replica holds, in hash order: after is the last
// hash of the previous page ("" for the first). It is what a sync offers peers.
func (s *Store) HeldBlobs(ctx context.Context, after string, limit int) ([]BlobRef, error) {
	bs, err := s.blobStore()
	if err != nil {
		if errors.Is(err, ErrNoBlobStore) {
			return nil, nil
		}
		return nil, err
	}
	var out []BlobRef
	for len(out) < limit {
		page, err := s.blobPage(ctx, after, 500)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			if bs.Has(r.Hash) {
				out = append(out, r)
				if len(out) == limit {
					return out, nil
				}
			}
		}
		after = page[len(page)-1].Hash
	}
	return out, nil
}

// blobPage is the distinct (hash, size) pairs of the attachments view after a hash, in hash order.
func (s *Store) blobPage(ctx context.Context, after string, n int) ([]BlobRef, error) {
	rows, err := s.read().QueryContext(ctx, `SELECT hash, MIN(size) FROM attachments WHERE hash > ? GROUP BY hash ORDER BY hash LIMIT ?`, after, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlobRef
	for rows.Next() {
		var r BlobRef
		if err := rows.Scan(&r.Hash, &r.Size); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BlobWanted reports whether an attachment op in the view names exactly this hash with exactly this size. A
// peer may upload a blob only if it is wanted: that is what stops an enrolled device from using this
// replica as free storage.
func (s *Store) BlobWanted(ctx context.Context, hash string, size int64) (bool, error) {
	var n int
	err := s.read().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM attachments WHERE hash = ? AND size = ?)`, hash, size).Scan(&n)
	return n == 1, err
}

// GCResult says what a collection did (or, for a dry run, would do).
type GCResult struct {
	DryRun     bool  `json:"dry_run"`
	Scanned    int   `json:"scanned"`    // blobs on disk
	Referenced int   `json:"referenced"` // kept: an op in the log names them
	Young      int   `json:"young"`      // kept: no op names them, but they are newer than the grace period
	Removed    int   `json:"removed"`
	Freed      int64 `json:"freed_bytes"`
}

// CollectBlobs removes blobs that no op in the log names, once they are older than grace.
//
//   - "Names" is judged against the whole op log, not the view: a blob that a deleted task's op, or an op
//     this replica will fold differently after a rebuild, still names is kept. Nothing removes an op, so a
//     blob an op names is kept forever; deleting a task does not free its photos (a policy decision, not
//     made here).
//   - Grace exists because the bytes and the op reach a replica separately, in either order, and a blob can
//     be stored before the op that names it exists here: a write in progress, an op still on another
//     device. Age is counted from when the blob was stored or last stored again (Put refreshes it), and
//     grace may not be under MinBlobGrace.
//   - It removes files only. Partial transfers are not touched (they have their own expiry).
func (s *Store) CollectBlobs(ctx context.Context, grace time.Duration, dryRun bool) (GCResult, error) {
	res := GCResult{DryRun: dryRun}
	if grace < MinBlobGrace {
		return res, ErrGraceTooShort
	}
	bs, err := s.blobStore()
	if err != nil {
		if errors.Is(err, ErrNoBlobStore) {
			return res, nil
		}
		return res, err
	}
	// The set of named hashes is read before the walk. A blob stored after this read is younger than grace.
	named := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT json_extract(payload, '$.hash') FROM ops WHERE kind = ?`, KindAttachmentAdded)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return res, err
		}
		named[h] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()
	cutoff := time.Now().Add(-grace)
	err = bs.Walk(func(h string, size int64, mod time.Time) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		res.Scanned++
		switch {
		case named[h]:
			res.Referenced++
		case mod.After(cutoff):
			res.Young++
		default:
			if !dryRun {
				if err := bs.Remove(h); err != nil {
					return err
				}
			}
			res.Removed++
			res.Freed += size
		}
		return nil
	})
	return res, err
}

// BlobStats is the blob side of the growth numbers.
type BlobStats struct {
	Blobs        int   `json:"blobs"` // files on disk
	Bytes        int64 `json:"bytes"`
	Referenced   int   `json:"referenced"` // distinct hashes the view names
	Missing      int   `json:"missing"`    // named but not here
	MissingBytes int64 `json:"missing_bytes"`
	Orphans      int   `json:"orphans"` // on disk, named by no op in the log
	OrphanBytes  int64 `json:"orphan_bytes"`
}

// BlobStats counts what the blob directory holds and what the log says it should.
func (s *Store) BlobStats(ctx context.Context) (BlobStats, error) {
	var st BlobStats
	bs, err := s.blobStore()
	if err != nil {
		if errors.Is(err, ErrNoBlobStore) {
			return st, nil
		}
		return st, err
	}
	named := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT json_extract(payload, '$.hash') FROM ops WHERE kind = ?`, KindAttachmentAdded)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return st, err
		}
		named[h] = true
	}
	rows.Close()
	err = bs.Walk(func(h string, size int64, _ time.Time) error {
		st.Blobs++
		st.Bytes += size
		if !named[h] {
			st.Orphans++
			st.OrphanBytes += size
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	after := ""
	for {
		page, err := s.blobPage(ctx, after, 500)
		if err != nil {
			return st, err
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			st.Referenced++
			if !bs.Has(r.Hash) {
				st.Missing++
				st.MissingBytes += r.Size
			}
		}
		after = page[len(page)-1].Hash
	}
	return st, nil
}
