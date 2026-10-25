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
		case KindNoteAdded, KindNoteRemoved:
			// an append or its removal, not a register
		case KindTaskRemoved:
			// whether a task is deleted is not a field: it depends on what the deleter had seen
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
	all, err := s.readAllOps(ctx)
	if err != nil {
		return nil, err
	}
	return FieldsFromOps(all)
}

// Notes reads the whole op log and returns the task notes as an OR-Set.
func (s *Store) Notes(ctx context.Context) (crdt.Set, error) {
	all, err := s.readAllOps(ctx)
	if err != nil {
		return crdt.Set{}, err
	}
	return NotesFromOps(all)
}

// readAllOps pages through the log, fieldsPageSize ops at a time.
func (s *Store) readAllOps(ctx context.Context) ([]Op, error) {
	var all []Op
	var after int64
	for {
		page, err := s.Ops(ctx, after, fieldsPageSize)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		all = append(all, page...)
		after = page[len(page)-1].Pos
	}
}

// NotesFromOps turns the note ops of a log into an OR-Set: each task.note_added
// is one add, tagged with the writing node and the op's HLC stamp, so two notes
// never share a tag even if their text is the same. Each note.removed
// tombstones the tag of the one note it names, so a note added at the same time
// on another replica (a different tag) survives. Pure and order-independent,
// like FieldsFromOps. Other known kinds are not notes and are skipped; an
// unknown kind is a corrupt log.
func NotesFromOps(ops []Op) (crdt.Set, error) {
	type id struct {
		node string
		seq  int64
	}
	tags := make(map[id]crdt.Tag, len(ops))
	set := crdt.NewSet(len(ops), len(ops)/4)
	for _, op := range ops {
		switch op.Kind {
		case KindNoteAdded:
			var p notePayload
			if err := json.Unmarshal(op.Payload, &p); err != nil {
				return crdt.Set{}, fmt.Errorf("%w: op %d: %v", ErrCorruptLog, op.Pos, err)
			}
			tag := crdt.Tag{Node: op.NodeID, Stamp: op.Stamp()}
			tags[id{op.NodeID, op.Seq}] = tag
			set.AddInPlace(crdt.NoteElement(p.ID, tag, p.Text), tag)
		case KindTaskCreated, KindTaskUpdated, KindTaskRemoved, KindNoteRemoved:
		default:
			return crdt.Set{}, fmt.Errorf("%w: op %d has unknown kind %q", ErrCorruptLog, op.Pos, op.Kind)
		}
	}
	// Removals are applied after every add is known, so the order of ops in the
	// slice does not matter.
	for _, op := range ops {
		if op.Kind != KindNoteRemoved {
			continue
		}
		var p noteRemovedPayload
		if err := json.Unmarshal(op.Payload, &p); err != nil {
			return crdt.Set{}, fmt.Errorf("%w: op %d: %v", ErrCorruptLog, op.Pos, err)
		}
		tag, ok := tags[id{p.Node, p.Seq}]
		if !ok {
			return crdt.Set{}, fmt.Errorf("%w: op %d removes a note (%s/%d) that is not in the log", ErrCorruptLog, op.Pos, p.Node, p.Seq)
		}
		set.RemoveTagsInPlace(tag)
	}
	return set, nil
}
