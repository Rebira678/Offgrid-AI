package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"

	"offgrid/internal/crdt"
	"offgrid/internal/task"
)

// FieldsFromOps turns a list of ops into last-writer-wins registers: one per
// task field, stamped with the op's hybrid logical timestamp and node. It is
// pure: the result depends only on the ops, in any order, because each write
// carries its own stamp and the map keeps the largest.
//
// Notes are not registers (they are appended, and become an OR-Set on Day 18),
// so note ops are skipped. Any other unknown kind is a corrupt log.
func FieldsFromOps(ops []Op) (crdt.Map, error) {
	m := make(crdt.Map, len(ops)*2)
	put := func(op Op, id, field, value string) {
		k := crdt.TaskKey(id, field)
		e := crdt.Entry{Value: value, Stamp: op.Stamp(), Node: op.NodeID}
		if cur, ok := m[k]; !ok || e.Compare(cur) > 0 {
			m[k] = e
		}
	}
	for _, op := range ops {
		switch op.Kind {
		case KindTaskCreated:
			var p createdPayload
			if err := json.Unmarshal(op.Payload, &p); err != nil {
				return nil, fmt.Errorf("%w: op %d: %v", ErrCorruptLog, op.Pos, err)
			}
			put(op, p.ID, crdt.FieldTitle, p.Title)
			put(op, p.ID, crdt.FieldSite, p.Site)
			put(op, p.ID, crdt.FieldPriority, string(p.Priority))
			put(op, p.ID, crdt.FieldStatus, string(task.StatusOpen))
		case KindTaskUpdated:
			var p updatedPayload
			if err := json.Unmarshal(op.Payload, &p); err != nil {
				return nil, fmt.Errorf("%w: op %d: %v", ErrCorruptLog, op.Pos, err)
			}
			if p.Status != nil {
				put(op, p.ID, crdt.FieldStatus, string(*p.Status))
			}
			if p.Priority != nil {
				put(op, p.ID, crdt.FieldPriority, string(*p.Priority))
			}
		case KindNoteAdded:
			// an append, not a register
		default:
			return nil, fmt.Errorf("%w: op %d has unknown kind %q", ErrCorruptLog, op.Pos, op.Kind)
		}
	}
	return m, nil
}

// fieldsPageSize is how many ops Fields reads at a time. A variable so a test can make it tiny.
var fieldsPageSize = 5000

// Fields reads the whole op log and returns its task fields as a
// last-writer-wins map. Two replicas that have exchanged their logs (Day 19)
// will hold maps that crdt.Merge brings to the same value.
func (s *Store) Fields(ctx context.Context) (crdt.Map, error) {
	var all []Op
	var after int64
	for {
		page, err := s.Ops(ctx, after, fieldsPageSize)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		after = page[len(page)-1].Pos
	}
	return FieldsFromOps(all)
}
