package sqlstore

// Signed ops.
//
// The hash chain (chain.go) shows that an op was not edited. It cannot show WHO
// wrote it: anyone who can build a valid chain can write any op. A signature
// fixes that. Each op's chain hash is signed by the device that wrote it, and
// the signature is stored beside the op (op_sigs) and travels with it.
//
// Per op, not per batch, on purpose: a relay can forward any subset of ops, in
// any grouping, without breaking a signature. The price is 64 bytes and one
// verification per op, which is measured, not guessed, in docs/day-36.md.
//
// What is signed is the chain hash, which already covers the op's replica,
// sequence number, timestamp, kind, payload and the previous op's hash. So one
// signature also vouches for the whole history before it, as that device saw it.
//
// Old history has no signatures. It is not pretended to: `store verify` lists
// it as unsigned unless an attestation covers it ("trusted as of the
// migration"), and says who attested and when.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"offgrid/internal/identity"
)

// Signer is a device key as the store needs it. *identity.Identity satisfies it.
type Signer interface {
	ID() string
	PublicKey() string // tagged: "ecdsa-p256:..."
	Algorithm() string
	Sign(msg []byte) ([]byte, error)
}

// OpSig is a signature as it travels with an op.
type OpSig struct {
	Alg    string `json:"alg"`           // algorithm tag, also stored per signature, so an algorithm can change later
	Device string `json:"device"`        // the signing device's ID (a hash of its public key)
	Sig    []byte `json:"sig"`           // 64 bytes for ecdsa-p256
	Key    string `json:"key,omitempty"` // the tagged public key; sent once per device per batch
}

var (
	// ErrBadSignature: a signature does not verify, or names a key it does not match.
	ErrBadSignature = errors.New("sync: an op's signature is not valid")
	// ErrKeyChange: a replica's ops are signed by a different device than the one that signed its first op.
	ErrKeyChange = errors.New("sync: an op is signed by a different device than this replica's earlier ops")
	// ErrUnsigned: an op has no signature, but its replica has signed before (or this replica requires signatures).
	ErrUnsigned = errors.New("sync: an op is not signed")
)

// SigError names the op whose signature (or lack of one) failed.
type SigError struct {
	Node   string
	Seq    int64
	Reason string
	Kind   error // ErrBadSignature, ErrKeyChange or ErrUnsigned
}

func (e *SigError) Error() string {
	return fmt.Sprintf("%v: op %s/%d: %s", e.Kind, e.Node, e.Seq, e.Reason)
}
func (e *SigError) Unwrap() error { return e.Kind }

func sigErr(kind error, node string, seq int64, format string, a ...any) error {
	return &SigError{Node: node, Seq: seq, Reason: fmt.Sprintf(format, a...), Kind: kind}
}

// sigDomain separates these signatures from any other use of the same key.
const sigDomain = "offgrid-opsig-v1\x00"

func sigMessage(hash string) []byte { return []byte(sigDomain + hash) }

// checkSigner makes sure the key a caller hands over is whole.
func checkSigner(sg Signer) error {
	id, err := identity.DeviceID(sg.PublicKey())
	if err != nil {
		return fmt.Errorf("sqlstore: signing key: %w", err)
	}
	if id != sg.ID() || !strings.HasPrefix(sg.PublicKey(), sg.Algorithm()+":") {
		return errors.New("sqlstore: signing key: the device ID or algorithm does not match the public key")
	}
	return nil
}

// boundDevice is the device that signs for node, or "" if none has yet.
func boundDevice(ctx context.Context, x execer, node string) (string, error) {
	var d string
	err := x.QueryRowContext(ctx, `SELECT device_id FROM node_devices WHERE node_id = ?`, node).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return d, err
}

// storeSig saves one signature, its key, and (first time only) the node's device binding.
func storeSig(ctx context.Context, x execer, node string, seq int64, sig OpSig, pub string) error {
	bound, err := boundDevice(ctx, x, node)
	if err != nil {
		return err
	}
	switch {
	case bound == "":
		if _, err := x.ExecContext(ctx, `INSERT INTO node_devices (node_id, device_id) VALUES (?, ?)`, node, sig.Device); err != nil {
			return err
		}
	case bound != sig.Device:
		return sigErr(ErrKeyChange, node, seq, "signed by %s, but %s signed this replica's earlier ops", sig.Device, bound)
	}
	if _, err := x.ExecContext(ctx, `INSERT OR IGNORE INTO device_keys (device_id, public_key) VALUES (?, ?)`, sig.Device, pub); err != nil {
		return err
	}
	_, err = x.ExecContext(ctx, `INSERT INTO op_sigs (node_id, seq, alg, device_id, sig) VALUES (?, ?, ?, ?, ?)`, node, seq, sig.Alg, sig.Device, sig.Sig)
	return err
}

// signLocal signs an op this replica has just written, or refuses if it has
// signed before and no key is loaded.
func (s *Store) signLocal(ctx context.Context, tx execer, op Op, hash string) error {
	if s.signer == nil {
		bound, err := boundDevice(ctx, tx, op.NodeID)
		if err != nil {
			return err
		}
		if bound != "" {
			return sigErr(ErrUnsigned, op.NodeID, op.Seq, "this replica's ops are signed by %s, but no device key is loaded: refusing to write an unsigned op (is the key file missing?)", bound)
		}
		return nil
	}
	sig, err := s.signer.Sign(sigMessage(hash))
	if err != nil {
		return fmt.Errorf("sqlstore: sign %s/%d: %w", op.NodeID, op.Seq, err)
	}
	return storeSig(ctx, tx, op.NodeID, op.Seq, OpSig{Alg: s.signer.Algorithm(), Device: s.signer.ID(), Sig: sig}, s.signer.PublicKey())
}

// checkBinding is run at Open: the key in hand must be the one this replica has always signed with.
func (s *Store) checkBinding(ctx context.Context) error {
	if s.signer == nil {
		return nil
	}
	bound, err := boundDevice(ctx, s.db, s.node)
	if err != nil {
		return err
	}
	if bound != "" && bound != s.signer.ID() {
		return fmt.Errorf("%w: this database's ops are signed by %s, but the key in use is %s (wrong key file? A replica keeps one device key; use a new database for a new device)", ErrKeyChange, bound, s.signer.ID())
	}
	return nil
}

// ---- verifying incoming ops ------------------------------------------------------------------

type sigJob struct {
	idx    int
	key    string
	msg    []byte
	sig    []byte
	node   string
	seq    int64
	device string
}

// verifyPool checks every job in a pool bounded by GOMAXPROCS and returns the
// error of the LOWEST index that failed (not the first to finish), so the op
// named is the same on every run. Nothing here touches the database: the
// caller's transaction stays single-writer.
func verifyPool(jobs []sigJob) (idx int, err error) {
	idx = -1
	if len(jobs) == 0 {
		return idx, nil
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > len(jobs) {
		workers = len(jobs)
	}
	errs := make([]error, len(jobs))
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				j := jobs[i]
				if verr := identity.Verify(j.key, j.msg, j.sig); verr != nil {
					errs[i] = sigErr(ErrBadSignature, j.node, j.seq, "signature by %s does not verify against the op's hash (%v)", j.device, verr)
				}
			}
		}()
	}
	for i := range jobs {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			return jobs[i].idx, e
		}
	}
	return -1, nil
}

// verifyFresh checks the signatures of ops about to be stored. fresh must be in
// (node, seq) order and continue each node's sequence, which ApplyRemote has
// already established. It returns the public key of each device seen. A refusal
// names the lowest-numbered op at fault.
func (s *Store) verifyFresh(ctx context.Context, tx execer, fresh []Op) (map[string]string, error) {
	keys := map[string]string{}
	bound := map[string]string{}
	var jobs []sigJob
	var structErr error
	structIdx := len(fresh)
	var prev string
	for i, op := range fresh {
		if i == 0 || fresh[i-1].NodeID != op.NodeID {
			p, err := chainPrev(ctx, tx, op.NodeID, op.Seq)
			if err != nil {
				return nil, err
			}
			prev = p
		}
		hash := OpHash(op, prev)
		prev = hash

		if _, ok := bound[op.NodeID]; !ok {
			d, err := boundDevice(ctx, tx, op.NodeID)
			if err != nil {
				return nil, err
			}
			bound[op.NodeID] = d
		}
		fail := func(e error) {
			if structErr == nil {
				structErr, structIdx = e, i
			}
		}
		if op.Sig == nil {
			switch {
			case bound[op.NodeID] != "":
				fail(sigErr(ErrUnsigned, op.NodeID, op.Seq, "%s signed this replica's earlier ops, so every later op must be signed", bound[op.NodeID]))
			case s.requireSigned:
				fail(sigErr(ErrUnsigned, op.NodeID, op.Seq, "this replica only accepts signed ops"))
			}
			if structErr != nil {
				break
			}
			continue
		}
		if structErr != nil {
			break // everything from here on is after the first structural failure
		}
		sg := op.Sig
		key := sg.Key
		if key != "" {
			dev, err := identity.DeviceID(key)
			if err != nil {
				fail(sigErr(ErrBadSignature, op.NodeID, op.Seq, "its public key is not usable: %v", err))
				break
			}
			if dev != sg.Device {
				fail(sigErr(ErrBadSignature, op.NodeID, op.Seq, "the public key sent with it does not belong to device %s", sg.Device))
				break
			}
			keys[dev] = key
		} else if k, ok := keys[sg.Device]; ok {
			key = k
		} else {
			var stored string
			err := tx.QueryRowContext(ctx, `SELECT public_key FROM device_keys WHERE device_id = ?`, sg.Device).Scan(&stored)
			if errors.Is(err, sql.ErrNoRows) {
				fail(sigErr(ErrBadSignature, op.NodeID, op.Seq, "no public key is known for device %s and none was sent", sg.Device))
				break
			} else if err != nil {
				return nil, err
			}
			key = stored
			keys[sg.Device] = stored
		}
		if !strings.HasPrefix(key, sg.Alg+":") {
			fail(sigErr(ErrBadSignature, op.NodeID, op.Seq, "it says it was signed with %q, which is not the algorithm of the device's key", sg.Alg))
			break
		}
		if b := bound[op.NodeID]; b == "" {
			bound[op.NodeID] = sg.Device
		} else if b != sg.Device {
			fail(sigErr(ErrKeyChange, op.NodeID, op.Seq, "signed by %s, but %s signed this replica's earlier ops", sg.Device, b))
			break
		}
		jobs = append(jobs, sigJob{idx: i, key: key, msg: sigMessage(hash), sig: sg.Sig, node: op.NodeID, seq: op.Seq, device: sg.Device})
	}
	badIdx, err := verifyPool(jobs)
	if err != nil && badIdx < structIdx {
		return nil, err
	}
	if structErr != nil {
		return nil, structErr
	}
	return keys, nil
}

// attachSigs fills in Sig (and Key, once per device) on ops about to be sent.
func (s *Store) attachSigs(ctx context.Context, ops []Op) error {
	type span struct{ lo, hi int64 }
	spans := map[string]*span{}
	for _, op := range ops {
		sp := spans[op.NodeID]
		if sp == nil {
			spans[op.NodeID] = &span{op.Seq, op.Seq}
			continue
		}
		if op.Seq < sp.lo {
			sp.lo = op.Seq
		}
		if op.Seq > sp.hi {
			sp.hi = op.Seq
		}
	}
	type id struct {
		node string
		seq  int64
	}
	found := map[id]OpSig{}
	for node, sp := range spans {
		rows, err := s.db.QueryContext(ctx, `SELECT seq, alg, device_id, sig FROM op_sigs WHERE node_id = ? AND seq BETWEEN ? AND ?`, node, sp.lo, sp.hi)
		if err != nil {
			return err
		}
		for rows.Next() {
			var seq int64
			var sg OpSig
			if err := rows.Scan(&seq, &sg.Alg, &sg.Device, &sg.Sig); err != nil {
				rows.Close()
				return err
			}
			found[id{node, seq}] = sg
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	sent := map[string]bool{}
	for i := range ops {
		sg, ok := found[id{ops[i].NodeID, ops[i].Seq}]
		if !ok {
			continue
		}
		if !sent[sg.Device] {
			var key string
			if err := s.db.QueryRowContext(ctx, `SELECT public_key FROM device_keys WHERE device_id = ?`, sg.Device).Scan(&key); err != nil {
				return fmt.Errorf("sqlstore: op %s/%d is signed by %s but its key is not stored: %w", ops[i].NodeID, ops[i].Seq, sg.Device, err)
			}
			sg.Key = key
			sent[sg.Device] = true
		}
		c := sg
		ops[i].Sig = &c
	}
	return nil
}

// ---- attestation: old history, trusted as of one moment --------------------------------------

type attestHead struct {
	Node string `json:"node"`
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

type attestPayload struct {
	Heads []attestHead `json:"heads"`
}

func decodeAttestation(raw json.RawMessage) (attestPayload, error) {
	var p attestPayload
	if err := decodeStrict(raw, &p); err != nil {
		return p, err
	}
	if len(p.Heads) > 4096 {
		return p, errors.New("too many replicas")
	}
	for _, h := range p.Heads {
		if !nodeIDRe.MatchString(h.Node) || h.Seq < 1 || len(h.Hash) != 64 || strings.Trim(h.Hash, "0123456789abcdef") != "" {
			return p, fmt.Errorf("head %q is not valid", h.Node)
		}
	}
	return p, nil
}

// Attestation is what Attest wrote.
type Attestation struct {
	By    string
	Heads []Head
}

// Attest signs the chain head of every replica this database holds right now,
// as one signed op, so that ops written before they could be signed are
// "trusted as of this moment, by this device". It is the honest answer for old
// history: nobody signed those ops when they were written, and nothing can
// change that; what can be said is who vouched for them, and when.
//
// It needs a device key, and it vouches only for what this replica holds: ops
// that arrive later are not covered. Until enrollment (Day 38) any device can
// attest; after it, only an admin's attestation will count.
func (s *Store) Attest(ctx context.Context) (Attestation, error) {
	var out Attestation
	if s.signer == nil {
		return out, errors.New("sqlstore: attest needs a device key (create one with `offgrid identity init`)")
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		heads, err := headsOf(ctx, tx)
		if err != nil {
			return err
		}
		p := attestPayload{Heads: make([]attestHead, len(heads))}
		for i, h := range heads {
			p.Heads[i] = attestHead(h)
		}
		out = Attestation{By: s.signer.ID(), Heads: heads}
		return s.record(ctx, tx, KindHistoryAttested, p)
	})
	return out, err
}

// ---- verifying everything stored ------------------------------------------------------------

// DeviceCount is how many ops a device signed.
type DeviceCount struct {
	Device string
	Ops    int
}

// AttestationInfo is one valid attestation found in the log.
type AttestationInfo struct {
	By     string
	Node   string // the replica that holds the attestation op
	Seq    int64
	At     time.Time
	Covers int // unsigned ops inside the heads it vouches for
}

// SigReport says what VerifySignatures checked.
type SigReport struct {
	Ops          int
	Signed       int
	Attested     int // unsigned ops covered by a valid attestation: "trusted as of the migration"
	Unsigned     int // unsigned ops nothing vouches for
	Devices      []DeviceCount
	Attestations []AttestationInfo
}

// VerifySignatures checks every stored signature against the op's stored chain
// hash. It returns a *SigError naming the first op (in replica, sequence order)
// whose signature fails, whose key is missing, that was signed by a device other
// than its replica's, or that is unsigned after its replica started signing.
// Run VerifyChain first (Verify does): a signature vouches for the stored hash,
// and VerifyChain proves the stored hash is the op's.
func (s *Store) VerifySignatures(ctx context.Context) (SigReport, error) {
	var rep SigReport
	rows, err := s.db.QueryContext(ctx, `SELECT c.node_id, c.seq, c.hash, o.kind, o.ts, s.alg, s.device_id, s.sig, k.public_key, n.device_id
		FROM op_chain c
		JOIN ops o ON o.node_id = c.node_id AND o.seq = c.seq
		LEFT JOIN op_sigs s ON s.node_id = c.node_id AND s.seq = c.seq
		LEFT JOIN device_keys k ON k.device_id = s.device_id
		LEFT JOIN node_devices n ON n.node_id = c.node_id
		ORDER BY c.node_id, c.seq`)
	if err != nil {
		return rep, err
	}
	type unsignedOp struct {
		node string
		seq  int64
	}
	type attOp struct {
		node   string
		seq    int64
		ts     int64
		device string
	}
	var (
		jobs      []sigJob
		unsigned  []unsignedOp
		atts      []attOp
		perDevice = map[string]int{}
		firstSign = map[string]int64{}
		firstBad  error
		firstOrd  int
	)
	note := func(e error) {
		if firstBad == nil {
			firstBad, firstOrd = e, rep.Ops
		}
	}
	for rows.Next() {
		var node, hash, kind string
		var seq, ts int64
		var alg, dev, key, bound sql.NullString
		var sig []byte
		if err := rows.Scan(&node, &seq, &hash, &kind, &ts, &alg, &dev, &sig, &key, &bound); err != nil {
			rows.Close()
			return rep, err
		}
		rep.Ops++
		if !dev.Valid {
			if f, ok := firstSign[node]; ok && seq > f {
				note(sigErr(ErrUnsigned, node, seq, "it is unsigned, but %s signed this replica's op %d: an op's signature must not be removed", bound.String, f))
			}
			unsigned = append(unsigned, unsignedOp{node, seq})
			continue
		}
		if _, ok := firstSign[node]; !ok {
			firstSign[node] = seq
		}
		rep.Signed++
		perDevice[dev.String]++
		if kind == KindHistoryAttested {
			atts = append(atts, attOp{node, seq, ts, dev.String})
		}
		switch {
		case !key.Valid:
			note(sigErr(ErrBadSignature, node, seq, "the public key of device %s is not stored", dev.String))
		case !bound.Valid || bound.String != dev.String:
			note(sigErr(ErrKeyChange, node, seq, "signed by %s, but this replica is bound to %s", dev.String, bound.String))
		case !strings.HasPrefix(key.String, alg.String+":"):
			note(sigErr(ErrBadSignature, node, seq, "it says %q, which is not the algorithm of the device's key", alg.String))
		default:
			if id, derr := identity.DeviceID(key.String); derr != nil || id != dev.String {
				note(sigErr(ErrBadSignature, node, seq, "the stored key does not belong to device %s", dev.String))
			} else {
				jobs = append(jobs, sigJob{idx: rep.Ops, key: key.String, msg: sigMessage(hash), sig: sig, node: node, seq: seq, device: dev.String})
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return rep, err
	}
	rows.Close()
	// Cheap structural failures were collected in order; a signature failure found
	// by the pool counts only if it comes before them.
	if ord, perr := verifyPool(jobs); perr != nil && (firstBad == nil || ord < firstOrd) {
		return rep, perr
	}
	if firstBad != nil {
		return rep, firstBad
	}

	for d, n := range perDevice {
		rep.Devices = append(rep.Devices, DeviceCount{d, n})
	}
	sort.Slice(rep.Devices, func(i, j int) bool { return rep.Devices[i].Device < rep.Devices[j].Device })

	// Attestations: valid when their signature verified (above) and the head they
	// name still matches the stored chain.
	covered := map[string]int64{}
	for _, a := range atts {
		var raw string
		if err := s.db.QueryRowContext(ctx, `SELECT payload FROM ops WHERE node_id = ? AND seq = ?`, a.node, a.seq).Scan(&raw); err != nil {
			return rep, err
		}
		p, err := decodeAttestation(json.RawMessage(raw))
		if err != nil {
			continue
		}
		reach := map[string]int64{}
		for _, h := range p.Heads {
			var mine string
			err := s.db.QueryRowContext(ctx, `SELECT hash FROM op_chain WHERE node_id = ? AND seq = ?`, h.Node, h.Seq).Scan(&mine)
			if err == nil && mine == h.Hash {
				reach[h.Node] = h.Seq
				if h.Seq > covered[h.Node] {
					covered[h.Node] = h.Seq
				}
			}
		}
		n := 0
		for _, u := range unsigned {
			if r, ok := reach[u.node]; ok && u.seq <= r {
				n++
			}
		}
		rep.Attestations = append(rep.Attestations, AttestationInfo{By: a.device, Node: a.node, Seq: a.seq, At: fromNanos(a.ts), Covers: n})
	}
	for _, u := range unsigned {
		if r, ok := covered[u.node]; ok && u.seq <= r {
			rep.Attested++
		} else {
			rep.Unsigned++
		}
	}
	return rep, nil
}
