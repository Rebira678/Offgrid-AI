package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// Day 44: hostile actions. With Config.Secure the replicas are a signed group (r0 founded it and enrolled
// the others, every replica signs its ops, RequireSigned is on) and the schedule also contains:
//
//	revoke  r0 revokes one replica's device (once per schedule), with the cutoff of what r0 holds of its chain
//	forge   someone who is not a member puts a bad op in front of a replica: an op altered in transit, an op
//	        from a device that was never enrolled, an honest op wearing a stranger's signature, an unsigned op
//	rogue   the revoked replica keeps working and hands its new ops to a peer
//	replay  a batch captured earlier is delivered again, possibly long after the world changed
//
// and the promises checked on top of the usual ones are: a forged op is refused, and the refusal changes
// nothing on the replica; the ops of a revoked device past its cutoff are void on every replica, and only
// those; a void op's effects are in no view; a replay of ops a replica already holds changes nothing. Generate
// puts at least one of each into every secure schedule, so "refused in every schedule" is a fact about the
// schedule and not a hope about the dice.
const (
	Revoke Kind = "revoke"
	Forge  Kind = "forge"
	Rogue  Kind = "rogue"
	Replay Kind = "replay"
)

var hostileKinds = []Kind{Revoke, Forge, Rogue, Replay}

// secureExtra are the hostile actions that also turn up at random (Revoke only where Generate puts it).
var secureExtra = []struct {
	k Kind
	w int
}{{Forge, 4}, {Rogue, 3}, {Replay, 4}}

func isHostile(k Kind) bool {
	for _, h := range hostileKinds {
		if k == h {
			return true
		}
	}
	return false
}

// securePrelude overwrites a few positions of a generated schedule so that every secure schedule holds a
// revocation, forgeries of every kind, rogue edits after the revocation, and replays before and after it.
func securePrelude(rng *rand.Rand, acts []Action, cfg Config) {
	n := len(acts)
	if n < 24 {
		return
	}
	taken := map[int]bool{}
	put := func(pos int, a Action) { // never on a position an earlier hostile action holds
		pos %= n
		for taken[pos] {
			pos = (pos + 1) % n
		}
		taken[pos] = true
		acts[pos] = a
	}
	rev := n/5 + rng.Intn(n/4)
	target := 1 + rng.Intn(cfg.Replicas-1)
	taken[rev] = true
	acts[rev] = Action{Kind: Revoke, A: 0, N: target}
	for i := 0; i < 5; i++ { // one forgery of each variant, anywhere
		put(n/3+rng.Intn(n-n/3), Action{Kind: Forge, A: rng.Intn(cfg.Replicas), N: i + 5*rng.Intn(1<<16)})
	}
	for i := 0; i < 3; i++ { // rogue edits after the revocation, handed to different peers
		pos := rev + 1 + rng.Intn(n-rev-1)
		put(pos, Action{Kind: Rogue, A: rng.Intn(cfg.Replicas), N: rng.Intn(1 << 20)})
	}
	put(rng.Intn(rev), Action{Kind: Replay, A: rng.Intn(cfg.Replicas), N: rng.Intn(1 << 20)})
	put(rev+1+rng.Intn(n-rev-1), Action{Kind: Replay, A: rng.Intn(cfg.Replicas), N: rng.Intn(1 << 20)})
}

// hostileState is what a secure world adds to a world.
type hostileState struct {
	ids      []*identity.Identity
	evil     *sqlstore.Store // a store whose key was never enrolled
	evilNode *sqlstore.Store // a store with no key at all
	revoked  map[int]bool
	captured [][]sqlstore.Op // batches that were delivered, for replay
}

func (w *world) openSecure(i int) (*sqlstore.Store, error) {
	return sqlstore.Open(context.Background(), w.path(i), sqlstore.Options{
		NodeID: nodeName(i), Now: w.clockOf(i), Signer: w.h.ids[i], RequireSigned: true,
	})
}

// startSecure founds the group on r0, enrols the others and lets them pull the membership and a first task.
func (w *world) startSecure() *Failure {
	ctx := context.Background()
	w.h.revoked = map[int]bool{}
	for range w.st {
		id, err := identity.Generate()
		if err != nil {
			return &Failure{"setup", err.Error()}
		}
		w.h.ids = append(w.h.ids, id)
	}
	for i := range w.st { // reopen with keys
		w.st[i].Close()
		s, err := w.openSecure(i)
		if err != nil {
			return &Failure{"setup", err.Error()}
		}
		w.st[i] = s
	}
	if err := w.st[0].Found(ctx, "r0"); err != nil {
		return &Failure{"setup", "found: " + err.Error()}
	}
	for i := 1; i < len(w.st); i++ {
		if err := w.st[0].AddMember(ctx, w.h.ids[i].PublicKey(), sqlstore.RoleMember, nodeName(i)); err != nil {
			return &Failure{"setup", "enrol: " + err.Error()}
		}
	}
	if _, _, err := w.st[0].Create(ctx, task.NewTask{Title: "first task", Site: "plant", Priority: task.PriorityMedium}); err != nil {
		return &Failure{"setup", err.Error()}
	}
	for i := 1; i < len(w.st); i++ {
		for {
			more, err := w.pull(i, 0, 50, faultNone)
			if err != nil {
				return &Failure{"setup", err.Error()}
			}
			if !more {
				break
			}
		}
	}
	w.log("start: a signed group of %d: r0 founded it and enrolled the rest; everyone holds the membership and a first task", len(w.st))
	return nil
}

// refused says an error is how a replica says no to an op it must not take.
func refused(err error) bool {
	for _, e := range []error{sqlstore.ErrNotAuthorized, sqlstore.ErrBadSignature, sqlstore.ErrUnsigned, sqlstore.ErrKeyChange,
		sqlstore.ErrBadOp, sqlstore.ErrChain, sqlstore.ErrFork, sqlstore.ErrGap} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// hostile runs one of the hostile actions.
func (w *world) hostile(a Action) *Failure {
	switch a.Kind {
	case Revoke:
		return w.doRevoke(a)
	case Forge:
		return w.doForge(a)
	case Rogue:
		return w.doRogue(a)
	case Replay:
		return w.doReplay(a)
	}
	return nil
}

func (w *world) doRevoke(a Action) *Failure {
	target := a.N
	if target < 1 || target >= len(w.st) {
		target = 1 + a.N%(len(w.st)-1)
	}
	if len(w.h.revoked) > 0 {
		w.log("r0 revoke: a device is already revoked in this schedule")
		return nil
	}
	rev, err := w.st[0].Revoke(context.Background(), w.h.ids[target].ID())
	if err != nil {
		return &Failure{"revoke", fmt.Sprintf("step %d: r0 could not revoke %s: %v", w.step, nodeName(target), err)}
	}
	w.h.revoked[target] = true
	w.stats.Revokes++
	w.log("r0 revoked %s; cutoffs %d", nodeName(target), len(rev.Cutoffs))
	return nil
}

// snapshot is what must not change when a replica says no.
func (w *world) snapshot(i int) (string, string, error) {
	ctx := context.Background()
	sum, err := w.st[i].Summary(ctx)
	if err != nil {
		return "", "", err
	}
	h, err := w.st[i].StateHash(ctx)
	return fmt.Sprint(sum), h, err
}

// preferNext picks, from ops that already exist, one this replica does not hold and could take next (so that the
// check is about the op and not about a gap), or any of them if there is none.
func (w *world) preferNext(i int, pool []sqlstore.Op, n int) sqlstore.Op {
	have, _ := w.st[i].Summary(context.Background())
	var next []sqlstore.Op
	for _, op := range pool {
		if op.Seq == have[op.NodeID]+1 {
			next = append(next, op)
		}
	}
	if len(next) > 0 {
		return next[n%len(next)]
	}
	return pool[n%len(pool)]
}

func (w *world) evilStores() error {
	ctx := context.Background()
	if w.h.evil == nil {
		id, err := identity.Generate()
		if err != nil {
			return err
		}
		if w.h.evil, err = sqlstore.Open(ctx, filepath.Join(w.dir, "evil.db"), sqlstore.Options{NodeID: "evil", Now: func() time.Time { return w.now }, Signer: id}); err != nil {
			return err
		}
	}
	if w.h.evilNode == nil {
		var err error
		if w.h.evilNode, err = sqlstore.Open(ctx, filepath.Join(w.dir, "evil2.db"), sqlstore.Options{NodeID: "evil2", Now: func() time.Time { return w.now }}); err != nil {
			return err
		}
	}
	return nil
}

func (w *world) doForge(a Action) *Failure {
	ctx := context.Background()
	fail := func(class, msg string) *Failure {
		return &Failure{class, fmt.Sprintf("step %d (%s): %s", w.step, a, msg)}
	}
	if err := w.evilStores(); err != nil {
		return fail("setup", err.Error())
	}
	var ops []sqlstore.Op
	variant := a.N % 5
	name := ""
	switch variant {
	case 0: // an honest op, altered on the way
		name = "an altered op"
		real, _, err := w.st[(a.A+1)%len(w.st)].OpsSince(ctx, sqlstore.Summary{}, 1<<30)
		if err != nil {
			return fail("forge", err.Error())
		}
		var pool []sqlstore.Op
		for _, op := range real {
			if op.Kind == sqlstore.KindTaskCreated || op.Kind == sqlstore.KindTaskUpdated || op.Kind == sqlstore.KindNoteAdded {
				pool = append(pool, op)
			}
		}
		if len(pool) == 0 {
			w.log("%s forge: no op to alter", nodeName(a.A))
			return nil
		}
		op := w.preferNext(a.A, pool, a.N/5)
		var p map[string]any
		if err := json.Unmarshal(op.Payload, &p); err != nil {
			return fail("forge", err.Error())
		}
		p["forged"] = a.N
		op.Payload, _ = json.Marshal(p)
		ops = []sqlstore.Op{op}
	case 1: // an op from a device nobody enrolled
		name = "an op from a stranger"
		if _, _, err := w.h.evil.Create(ctx, task.NewTask{Title: fmt.Sprintf("forged %d", w.nth), Site: "plant", Priority: task.PriorityHigh}); err != nil {
			return fail("forge", err.Error())
		}
		var err error
		if ops, _, err = w.h.evil.OpsSince(ctx, sqlstore.Summary{}, 1<<30); err != nil {
			return fail("forge", err.Error())
		}
	case 2: // an honest op wearing a stranger's signature
		name = "an op with a stranger's signature"
		if _, _, err := w.h.evil.Create(ctx, task.NewTask{Title: fmt.Sprintf("forged sig %d", w.nth), Site: "plant", Priority: task.PriorityHigh}); err != nil {
			return fail("forge", err.Error())
		}
		evilOps, _, err := w.h.evil.OpsSince(ctx, sqlstore.Summary{}, 1<<30)
		if err != nil || len(evilOps) == 0 || evilOps[0].Sig == nil {
			return fail("forge", fmt.Sprintf("no stranger signature to borrow: %v", err))
		}
		real, _, err := w.st[(a.A+1)%len(w.st)].OpsSince(ctx, sqlstore.Summary{}, 1<<30)
		if err != nil {
			return fail("forge", err.Error())
		}
		var pool []sqlstore.Op
		for _, op := range real {
			if op.Sig != nil && op.Kind != sqlstore.KindMemberAdd {
				pool = append(pool, op)
			}
		}
		if len(pool) == 0 {
			w.log("%s forge: no signed op to re-sign", nodeName(a.A))
			return nil
		}
		op := w.preferNext(a.A, pool, a.N/5)
		op.Sig = evilOps[0].Sig
		ops = []sqlstore.Op{op}
	case 3: // an enrolled device's op with one bit of its signature flipped: the key is right, the signature is not
		name = "an op with a damaged signature"
		real, _, err := w.st[(a.A+1)%len(w.st)].OpsSince(ctx, sqlstore.Summary{}, 1<<30)
		if err != nil {
			return fail("forge", err.Error())
		}
		var pool []sqlstore.Op
		for _, op := range real {
			if op.Sig != nil && len(op.Sig.Sig) > 0 && op.Kind != sqlstore.KindMemberAdd {
				pool = append(pool, op)
			}
		}
		if len(pool) == 0 {
			w.log("%s forge: no signed op to damage", nodeName(a.A))
			return nil
		}
		op := w.preferNext(a.A, pool, a.N/5)
		sg := *op.Sig
		sg.Sig = append([]byte(nil), sg.Sig...)
		sg.Sig[0] ^= 0x01
		op.Sig = &sg
		ops = []sqlstore.Op{op}
	default: // an op with no signature at all
		name = "an unsigned op"
		if _, _, err := w.h.evilNode.Create(ctx, task.NewTask{Title: fmt.Sprintf("unsigned %d", w.nth), Site: "plant", Priority: task.PriorityHigh}); err != nil {
			return fail("forge", err.Error())
		}
		var err error
		if ops, _, err = w.h.evilNode.OpsSince(ctx, sqlstore.Summary{}, 1<<30); err != nil {
			return fail("forge", err.Error())
		}
	}
	w.stats.Forged++
	sumBefore, hashBefore, err := w.snapshot(a.A)
	if err != nil {
		return fail("forge", err.Error())
	}
	have, err := w.st[a.A].Summary(ctx)
	if err != nil {
		return fail("forge", err.Error())
	}
	heldAll := true
	for _, op := range ops {
		if have[op.NodeID] < op.Seq {
			heldAll = false
		}
	}
	_, err = w.st[a.A].ApplyRemote(ctx, ops)
	if err == nil {
		// An op the replica already holds is a duplicate: it is recognised by its place and hash, which the
		// stranger's signature is not part of, and ignored. Nothing is stored, so nothing is accepted.
		if heldAll {
			if sumAfter, hashAfter, e := w.snapshot(a.A); e == nil && sumAfter == sumBefore && hashAfter == hashBefore {
				w.stats.ForgedIgnored++
				w.log("%s ignored %s: it already held that op", nodeName(a.A), name)
				return nil
			}
		}
		return fail("forged-accepted", fmt.Sprintf("%s was taken in by %s", name, nodeName(a.A)))
	}
	if !refused(err) && !errors.Is(err, sqlstore.ErrClockDrift) {
		return fail("forge", fmt.Sprintf("%s was refused with an unexpected error: %v", name, err))
	}
	sumAfter, hashAfter, err2 := w.snapshot(a.A)
	if err2 != nil {
		return fail("forge", err2.Error())
	}
	if sumAfter != sumBefore || hashAfter != hashBefore {
		return fail("forged-changed", fmt.Sprintf("%s was refused (%v) but changed %s", name, err, nodeName(a.A)))
	}
	w.stats.ForgedRefused++
	w.log("%s refused %s: %v", nodeName(a.A), name, err)
	return nil
}

// doRogue: the revoked replica keeps working and hands what it has to a peer, reliably.
func (w *world) doRogue(a Action) *Failure {
	ctx := context.Background()
	x := -1
	for i := range w.h.revoked {
		if x < 0 || i < x {
			x = i
		}
	}
	if x < 0 {
		w.log("rogue: nobody is revoked yet")
		return nil
	}
	w.stats.Rogue++
	_, _, err := w.st[x].Create(ctx, task.NewTask{Title: fmt.Sprintf("rogue %d", w.nth), Site: "plant", Priority: task.PriorityHigh})
	if err != nil {
		if refused(err) {
			w.stats.RogueRefused++
			w.log("%s (revoked) could not write: %v", nodeName(x), err)
		} else if !benign(err) {
			return &Failure{"rogue", fmt.Sprintf("step %d: %v", w.step, err)}
		}
	}
	peer := a.A
	if peer == x {
		peer = (x + 1) % len(w.st)
	}
	for {
		more, err := w.pull(peer, x, 16, faultNone)
		if err != nil {
			if refused(err) || errors.Is(err, sqlstore.ErrClockDrift) { // a replica may also refuse the batch; settle then has to cope
				w.log("%s refused the revoked %s's batch: %v", nodeName(peer), nodeName(x), err)
				return nil
			}
			return syncFailure(fmt.Sprintf("step %d: rogue batch to %s", w.step, nodeName(peer)), err)
		}
		if !more {
			break
		}
	}
	w.log("%s took in what the revoked %s had", nodeName(peer), nodeName(x))
	return nil
}

// doReplay delivers a captured batch again. If the replica already holds everything in it, nothing may change.
func (w *world) doReplay(a Action) *Failure {
	ctx := context.Background()
	if len(w.h.captured) == 0 {
		w.log("%s replay: nothing captured yet", nodeName(a.A))
		return nil
	}
	batch := w.h.captured[a.N%len(w.h.captured)]
	have, err := w.st[a.A].Summary(ctx)
	if err != nil {
		return &Failure{"replay", err.Error()}
	}
	held := true
	for _, op := range batch {
		if have[op.NodeID] < op.Seq {
			held = false
		}
	}
	w.stats.Replays++
	sumBefore, hashBefore, err := w.snapshot(a.A)
	if err != nil {
		return &Failure{"replay", err.Error()}
	}
	_, err = w.st[a.A].ApplyRemote(ctx, batch)
	sumAfter, hashAfter, err2 := w.snapshot(a.A)
	if err2 != nil {
		return &Failure{"replay", err2.Error()}
	}
	if held {
		w.stats.ReplayHeld++
		if err != nil {
			return &Failure{"replay", fmt.Sprintf("step %d: replaying %d ops %s already holds was refused: %v", w.step, len(batch), nodeName(a.A), err)}
		}
		if sumAfter != sumBefore || hashAfter != hashBefore {
			return &Failure{"replay-changed", fmt.Sprintf("step %d: replaying %d ops %s already holds changed it", w.step, len(batch), nodeName(a.A))}
		}
		w.log("%s took a replay of %d ops it holds: nothing changed", nodeName(a.A), len(batch))
		return nil
	}
	if err != nil {
		if !(refused(err) || errors.Is(err, sqlstore.ErrClockDrift)) {
			return &Failure{"replay", fmt.Sprintf("step %d: %v", w.step, err)}
		}
		if sumAfter != sumBefore || hashAfter != hashBefore {
			return &Failure{"atomic", fmt.Sprintf("step %d: a refused replay (%v) changed %s", w.step, err, nodeName(a.A))}
		}
	}
	w.log("%s was given a batch it only partly holds: err=%v", nodeName(a.A), err)
	return nil
}

func (w *world) capture(ops []sqlstore.Op) {
	if !w.cfg.Secure || len(ops) == 0 {
		return
	}
	w.h.captured = append(w.h.captured, append([]sqlstore.Op(nil), ops...))
	if len(w.h.captured) > 128 {
		w.h.captured = w.h.captured[1:]
	}
}

func (w *world) closeHostile() {
	if w.h.evil != nil {
		w.h.evil.Close()
	}
	if w.h.evilNode != nil {
		w.h.evilNode.Close()
	}
}

// checkHostile is run when the replicas have converged.
func (w *world) checkHostile() *Failure {
	ctx := context.Background()
	all, _, err := w.st[0].OpsSince(ctx, sqlstore.Summary{}, 1<<30)
	if err != nil {
		return &Failure{"hostile", err.Error()}
	}
	for _, op := range all {
		if op.NodeID == "evil" || op.NodeID == "evil2" {
			return &Failure{"forged-stored", fmt.Sprintf("an op from %s/%d is in the log", op.NodeID, op.Seq)}
		}
	}
	rep, err := w.st[0].Verify(ctx)
	if err != nil {
		return &Failure{"verify", err.Error()}
	}
	if rep.Sigs.Unsigned != 0 {
		return &Failure{"unsigned", fmt.Sprintf("%d ops in the log are not signed", rep.Sigs.Unsigned)}
	}
	members, err := w.st[0].Members(ctx)
	if err != nil {
		return &Failure{"hostile", err.Error()}
	}
	want := map[[2]string]bool{} // node, seq -> must be void
	wantN := 0
	for i := range w.h.revoked {
		dev := w.h.ids[i].ID()
		var m *sqlstore.Member
		for k := range members.Members {
			if members.Members[k].Device == dev {
				m = &members.Members[k]
			}
		}
		if m == nil || !m.Revoked {
			return &Failure{"revoke", fmt.Sprintf("%s was revoked by r0 but the converged membership does not say so", nodeName(i))}
		}
		cut := int64(0)
		for _, c := range m.Cutoffs {
			if c.Node == nodeName(i) {
				cut = c.Seq
			}
		}
		for _, op := range all {
			if op.NodeID == nodeName(i) && op.Seq > cut {
				want[[2]string{op.NodeID, fmt.Sprint(op.Seq)}] = true
				wantN++
			}
		}
	}
	for i, s := range w.st {
		void, err := s.VoidOps(ctx)
		if err != nil {
			return &Failure{"hostile", err.Error()}
		}
		got := map[[2]string]bool{}
		for _, v := range void {
			got[[2]string{v.Node, fmt.Sprint(v.Seq)}] = true
		}
		var missing, extra []string
		for k := range want {
			if !got[k] {
				missing = append(missing, k[0]+"/"+k[1])
			}
		}
		for k := range got {
			if !want[k] {
				extra = append(extra, k[0]+"/"+k[1])
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 {
			return &Failure{"revoked-effective", fmt.Sprintf("r%d: ops of a revoked device past its cutoff still count: %v", i, missing)}
		}
		if len(extra) > 0 {
			return &Failure{"void-too-much", fmt.Sprintf("r%d: ops that should count were voided: %v", i, extra)}
		}
		if i == 0 {
			w.stats.VoidOps = len(void)
		}
	}
	// the effects of a void op are in no view
	void, _ := w.st[0].VoidOps(ctx)
	if len(void) > 0 {
		isVoid := map[[2]string]bool{}
		for _, v := range void {
			isVoid[[2]string{v.Node, fmt.Sprint(v.Seq)}] = true
		}
		live, err := w.st[0].List(ctx, task.Filter{})
		if err != nil {
			return &Failure{"hostile", err.Error()}
		}
		have := map[string]bool{}
		for _, t := range live {
			have[t.ID] = true
		}
		for _, op := range all {
			if op.Kind != sqlstore.KindTaskCreated || !isVoid[[2]string{op.NodeID, fmt.Sprint(op.Seq)}] {
				continue
			}
			var p opPayload
			if json.Unmarshal(op.Payload, &p) == nil && have[p.ID] {
				return &Failure{"void-visible", fmt.Sprintf("task %s, created by the void op %s/%d, is in the view", p.ID, op.NodeID, op.Seq)}
			}
		}
	}
	return nil
}
