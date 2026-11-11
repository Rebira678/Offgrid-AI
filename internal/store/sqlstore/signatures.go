package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"

	"offgrid/internal/identity"
)

// ErrBadSignature is returned when an op's signature is missing, invalid, or forged.
var ErrBadSignature = errors.New("invalid op signature")

// metaGenesisAttestedOps records how many ops were attested at migration.
const metaGenesisAttestedOps = "genesis_attested_ops"

// GenesisAttestation is a cryptographic attestation of a replica's head at migration,
// signed by an admin device key. It bridges pre-signing history into the signed log.
type GenesisAttestation struct {
	NodeID    string `json:"node"`
	Seq       int64  `json:"seq"`
	Hash      string `json:"hash"`
	DeviceID  string `json:"device_id"`
	Algorithm string `json:"algorithm"`
	PubKey    string `json:"pub_key"`
	Sig       []byte `json:"sig"`
}

// OpSignature is an individual op signature recorded in op_sigs.
type OpSignature struct {
	NodeID    string `json:"node"`
	Seq       int64  `json:"seq"`
	DeviceID  string `json:"device_id"`
	Algorithm string `json:"algorithm"`
	PubKey    string `json:"pub_key"`
	Sig       []byte `json:"sig"`
}

// SignatureReport summarizes signature verification results.
type SignatureReport struct {
	SignedOps    int                  // ops verified via individual op_sigs rows
	AttestedOps  int                  // legacy ops verified and trusted via genesis attestations
	Attestations []GenesisAttestation // replica genesis attestations verified
}

// GenesisAttestationMessage creates the canonical byte sequence signed for a genesis attestation.
func GenesisAttestationMessage(nodeID string, seq int64, headHash string) []byte {
	return []byte(fmt.Sprintf("offgrid-genesis-v1:%s:%d:%s", nodeID, seq, headHash))
}

// backfillGenesisAttestations inspects whether there are historical un-attested ops
// that exist without signatures (e.g. when upgrading a database created before Day 36).
// It signs an attestation for each replica's head using this device's identity key.
func (s *Store) backfillGenesisAttestations(ctx context.Context) error {
	if s.id == nil {
		return errors.New("sqlstore: cannot backfill genesis attestations without identity key")
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT o.node_id, MAX(o.seq)
		FROM ops o
		LEFT JOIN op_sigs s ON s.node_id = o.node_id AND s.seq = o.seq
		LEFT JOIN genesis_attestations g ON g.node_id = o.node_id
		WHERE s.sig IS NULL AND g.node_id IS NULL
		GROUP BY o.node_id
	`)
	if err != nil {
		return fmt.Errorf("sqlstore: query un-attested replicas: %w", err)
	}
	defer rows.Close()

	type unAttestedHead struct {
		node string
		seq  int64
	}
	var targets []unAttestedHead
	for rows.Next() {
		var h unAttestedHead
		if err := rows.Scan(&h.node, &h.seq); err != nil {
			return err
		}
		targets = append(targets, h)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	totalAttestedOps := 0
	for _, h := range targets {
		var headHash string
		err = tx.QueryRowContext(ctx, `SELECT hash FROM op_chain WHERE node_id = ? AND seq = ?`, h.node, h.seq).Scan(&headHash)
		if err != nil {
			return fmt.Errorf("sqlstore: find head hash for %s/%d during genesis attestation: %w", h.node, h.seq, err)
		}
		msg := GenesisAttestationMessage(h.node, h.seq, headHash)
		sig, err := s.id.Sign(msg)
		if err != nil {
			return fmt.Errorf("sqlstore: sign genesis attestation for %s/%d: %w", h.node, h.seq, err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO genesis_attestations (node_id, seq, hash, device_id, algorithm, pub_key, sig)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, h.node, h.seq, headHash, s.id.ID(), s.id.Algorithm(), s.id.PublicKey(), sig)
		if err != nil {
			return fmt.Errorf("sqlstore: store genesis attestation for %s/%d: %w", h.node, h.seq, err)
		}
		totalAttestedOps += int(h.seq)
	}

	prior := 0
	if v, ok, gerr := getMeta(ctx, tx, metaGenesisAttestedOps); gerr != nil {
		return gerr
	} else if ok {
		prior, _ = strconv.Atoi(v)
	}
	if err = setMeta(ctx, tx, metaGenesisAttestedOps, strconv.Itoa(prior+totalAttestedOps)); err != nil {
		return err
	}

	return tx.Commit()
}

// getGenesisAttestations reads all stored genesis attestations from the database.
func (s *Store) getGenesisAttestations(ctx context.Context) ([]GenesisAttestation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, seq, hash, device_id, algorithm, pub_key, sig FROM genesis_attestations ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GenesisAttestation
	for rows.Next() {
		var g GenesisAttestation
		if err := rows.Scan(&g.NodeID, &g.Seq, &g.Hash, &g.DeviceID, &g.Algorithm, &g.PubKey, &g.Sig); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// getGenesisAttestationSeqs returns a map from replica node_id to the cutoff sequence number
// covered by that replica's genesis attestation.
func (s *Store) getGenesisAttestationSeqs(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, seq FROM genesis_attestations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var node string
		var seq int64
		if err := rows.Scan(&node, &seq); err != nil {
			return nil, err
		}
		out[node] = seq
	}
	return out, rows.Err()
}

// verifyBatchSignatures verifies signatures on a batch of ops in parallel
// using a worker pool bounded by runtime.GOMAXPROCS(0).
func (s *Store) verifyBatchSignatures(ctx context.Context, ops []Op) error {
	var toVerify []Op
	for _, op := range ops {
		if len(op.Sig) == 0 {
			if s.opt.RequireSignedOps {
				return &TamperError{Node: op.NodeID, Seq: op.Seq, Reason: fmt.Sprintf("%v: missing signature", ErrBadSignature)}
			}
			continue
		}
		if op.PubKey == "" {
			return &TamperError{Node: op.NodeID, Seq: op.Seq, Reason: fmt.Sprintf("%v: missing public key", ErrBadSignature)}
		}
		if len(op.Sig) != 64 {
			return &TamperError{Node: op.NodeID, Seq: op.Seq, Reason: fmt.Sprintf("%v: signature is not 64 bytes", ErrBadSignature)}
		}
		if op.Alg != "" && op.Alg != identity.AlgECDSAP256 {
			return &TamperError{Node: op.NodeID, Seq: op.Seq, Reason: fmt.Sprintf("unsupported signature algorithm %q", op.Alg)}
		}
		if op.DeviceID != "" && op.PubKey != "" {
			expectedDevID, err := identity.DeviceID(op.PubKey)
			if err != nil {
				return &TamperError{Node: op.NodeID, Seq: op.Seq, Reason: fmt.Sprintf("invalid public key: %v", err)}
			}
			if op.DeviceID != expectedDevID {
				return &TamperError{Node: op.NodeID, Seq: op.Seq, Reason: fmt.Sprintf("device ID does not match public key (have %s, want %s)", op.DeviceID, expectedDevID)}
			}
		}
		toVerify = append(toVerify, op)
	}

	if len(toVerify) == 0 {
		return nil
	}

	if len(toVerify) == 1 {
		op := toVerify[0]
		if err := identity.Verify(op.PubKey, []byte(op.Hash), op.Sig); err != nil {
			return &TamperError{
				Node:   op.NodeID,
				Seq:    op.Seq,
				Reason: fmt.Sprintf("forged or edited signature: verification failed against %s: %v", op.PubKey, err),
			}
		}
		return nil
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > len(toVerify) {
		workers = len(toVerify)
	}
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan Op, len(toVerify))
	for _, op := range toVerify {
		jobs <- op
	}
	close(jobs)

	var once sync.Once
	var firstErr error
	var wg sync.WaitGroup
	ctxWorker, cancel := context.WithCancel(ctx)
	defer cancel()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for op := range jobs {
				select {
				case <-ctxWorker.Done():
					return
				default:
				}
				if verr := identity.Verify(op.PubKey, []byte(op.Hash), op.Sig); verr != nil {
					once.Do(func() {
						firstErr = &TamperError{
							Node:   op.NodeID,
							Seq:    op.Seq,
							Reason: fmt.Sprintf("forged or edited signature: verification failed against %s: %v", op.PubKey, verr),
						}
						cancel()
					})
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// VerifySignatures verifies all genesis attestations and op signatures stored in the database.
func (s *Store) VerifySignatures(ctx context.Context) (SignatureReport, error) {
	var rep SignatureReport

	// 1. Verify genesis attestations
	attestations, err := s.getGenesisAttestations(ctx)
	if err != nil {
		return rep, err
	}
	for _, g := range attestations {
		if g.DeviceID != "" && g.PubKey != "" {
			expectedDevID, err := identity.DeviceID(g.PubKey)
			if err != nil {
				return rep, &TamperError{Node: g.NodeID, Seq: g.Seq, Reason: fmt.Sprintf("genesis attestation invalid public key: %v", err)}
			}
			if g.DeviceID != expectedDevID {
				return rep, &TamperError{Node: g.NodeID, Seq: g.Seq, Reason: fmt.Sprintf("genesis attestation device ID does not match public key (have %s, want %s)", g.DeviceID, expectedDevID)}
			}
		}
		msg := GenesisAttestationMessage(g.NodeID, g.Seq, g.Hash)
		if verr := identity.Verify(g.PubKey, msg, g.Sig); verr != nil {
			return rep, &TamperError{Node: g.NodeID, Seq: g.Seq, Reason: fmt.Sprintf("genesis attestation signature does not verify: %v", verr)}
		}
		var chainHash string
		cerr := s.db.QueryRowContext(ctx, `SELECT hash FROM op_chain WHERE node_id = ? AND seq = ?`, g.NodeID, g.Seq).Scan(&chainHash)
		if cerr != nil {
			return rep, &TamperError{Node: g.NodeID, Seq: g.Seq, Reason: "genesis attestation head is missing from the hash chain"}
		}
		if chainHash != g.Hash {
			return rep, &TamperError{Node: g.NodeID, Seq: g.Seq, Reason: "genesis attestation hash does not match op_chain"}
		}
		rep.Attestations = append(rep.Attestations, g)
	}

	attestedSeqs := make(map[string]int64, len(attestations))
	for _, g := range attestations {
		attestedSeqs[g.NodeID] = g.Seq
	}

	var sigCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM op_sigs`).Scan(&sigCount); err != nil {
		return rep, err
	}
	if len(attestations) == 0 && sigCount == 0 && !s.opt.RequireSignedOps {
		return rep, nil
	}

	// 2. Verify individual signatures
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.node_id, o.seq, COALESCE(c.hash, ''), s.sig, COALESCE(s.pub_key, ''), COALESCE(s.device_id, ''), COALESCE(s.algorithm, '')
		FROM ops o
		LEFT JOIN op_chain c ON c.node_id = o.node_id AND c.seq = o.seq
		LEFT JOIN op_sigs s ON s.node_id = o.node_id AND s.seq = o.seq
		ORDER BY o.node_id, o.seq
	`)
	if err != nil {
		return rep, err
	}
	defer rows.Close()

	for rows.Next() {
		var node string
		var seq int64
		var hash, pubKey, devID, alg string
		var sig []byte
		if err := rows.Scan(&node, &seq, &hash, &sig, &pubKey, &devID, &alg); err != nil {
			return rep, err
		}
		if cutoff, ok := attestedSeqs[node]; ok && seq <= cutoff {
			rep.AttestedOps++
			continue
		}
		if len(sig) == 0 {
			if s.opt.RequireSignedOps || len(attestations) > 0 {
				return rep, &TamperError{Node: node, Seq: seq, Reason: "has no entry in the op_sigs table"}
			}
			continue
		}
		if len(sig) != 64 {
			return rep, &TamperError{Node: node, Seq: seq, Reason: "forged or edited signature: signature is not 64 bytes"}
		}
		if alg != identity.AlgECDSAP256 {
			return rep, &TamperError{Node: node, Seq: seq, Reason: fmt.Sprintf("unsupported signature algorithm %q", alg)}
		}
		if devID != "" && pubKey != "" {
			expectedDevID, err := identity.DeviceID(pubKey)
			if err != nil {
				return rep, &TamperError{Node: node, Seq: seq, Reason: fmt.Sprintf("invalid public key: %v", err)}
			}
			if devID != expectedDevID {
				return rep, &TamperError{Node: node, Seq: seq, Reason: fmt.Sprintf("device ID does not match public key (have %s, want %s)", devID, expectedDevID)}
			}
		}
		if verr := identity.Verify(pubKey, []byte(hash), sig); verr != nil {
			return rep, &TamperError{Node: node, Seq: seq, Reason: fmt.Sprintf("forged or edited signature: does not verify: %v", verr)}
		}
		rep.SignedOps++
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}

	// 3. Detect orphan signatures (signature present without corresponding op)
	var orphanNode string
	var orphanSeq int64
	err = s.db.QueryRowContext(ctx, `
		SELECT s.node_id, s.seq FROM op_sigs s
		LEFT JOIN ops o ON o.node_id = s.node_id AND o.seq = s.seq
		WHERE o.pos IS NULL LIMIT 1
	`).Scan(&orphanNode, &orphanSeq)
	switch {
	case err == nil:
		return rep, &TamperError{Node: orphanNode, Seq: orphanSeq, Reason: "has a signature but no op: the op was removed from the log"}
	case !errors.Is(err, sql.ErrNoRows):
		return rep, err
	}

	return rep, nil
}
