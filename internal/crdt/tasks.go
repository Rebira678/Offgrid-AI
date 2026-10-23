package crdt

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"offgrid/internal/hlc"
)

// A task is not one register but several: one per field. Keys are
// "task/<id>/<field>", so a Map holds every field of every task.
//
// The fields are the ones a person can change on a task: title, site,
// priority and status. Notes are not here: they are appended, never
// overwritten, and need a different type (the OR-Set, Day 18).

// The task fields held as registers.
const (
	FieldTitle    = "title"
	FieldSite     = "site"
	FieldPriority = "priority"
	FieldStatus   = "status"
)

const taskPrefix = "task/"

// TaskKey is the map key for one field of one task.
func TaskKey(id, field string) string { return taskPrefix + id + "/" + field }

// SplitTaskKey is the inverse of TaskKey. ok is false for any other key.
// Task IDs never contain "/", so the last slash separates the field.
func SplitTaskKey(key string) (id, field string, ok bool) {
	rest, found := strings.CutPrefix(key, taskPrefix)
	if !found {
		return "", "", false
	}
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// TaskFields reads the live fields of every task out of m: task ID to field
// name to value. A task whose fields are all deleted does not appear.
func TaskFields(m Map) map[string]map[string]string {
	out := make(map[string]map[string]string, len(m)/2)
	for k, e := range m {
		id, field, ok := SplitTaskKey(k)
		if !ok || e.Deleted {
			continue
		}
		if out[id] == nil {
			out[id] = make(map[string]string, 4)
		}
		out[id][field] = e.Value
	}
	return out
}

// ---- notes: an OR-Set of add events -------------------------------------
//
// A note is added, never overwritten, so it cannot live in a last-writer-wins
// register. Each note is one element of an OR-Set. Two people who add the same
// text are adding two notes, so the element embeds the unique tag of its add:
//
//	note/<task id>/<wall>.<counter>.<node>\n<text>
//
// The wall and counter are fixed-width so the elements sort by time; the text
// may contain anything, including newlines (the first newline ends the key).

// NoteElement builds the set element for one note.
func NoteElement(taskID string, tag Tag, text string) string {
	return fmt.Sprintf("note/%s/%020d.%010d.%s\n%s", taskID, tag.Stamp.Wall, tag.Stamp.Counter, tag.Node, text)
}

// SplitNoteElement is the inverse of NoteElement. ok is false for anything else.
func SplitNoteElement(elem string) (taskID string, tag Tag, text string, ok bool) {
	rest, found := strings.CutPrefix(elem, "note/")
	if !found {
		return "", Tag{}, "", false
	}
	key, text, found := strings.Cut(rest, "\n")
	if !found {
		return "", Tag{}, "", false
	}
	i := strings.LastIndexByte(key, '/')
	if i <= 0 {
		return "", Tag{}, "", false
	}
	taskID, stamp := key[:i], key[i+1:]
	// 20 digits, '.', 10 digits, '.', node
	if len(stamp) < 33 || stamp[20] != '.' || stamp[31] != '.' {
		return "", Tag{}, "", false
	}
	wall, err1 := strconv.ParseInt(stamp[:20], 10, 64)
	counter, err2 := strconv.ParseUint(stamp[21:31], 10, 32)
	if err1 != nil || err2 != nil {
		return "", Tag{}, "", false
	}
	return taskID, Tag{Node: stamp[32:], Stamp: hlc.Timestamp{Wall: wall, Counter: uint32(counter)}}, text, true
}

// Note is one live note of a task.
type Note struct {
	Text string
	Tag  Tag
}

// TaskNotes reads the live notes of every task out of s, oldest first (by
// stamp, then node). Elements that are not notes are ignored.
func TaskNotes(s Set) map[string][]Note {
	elems := s.Elements()
	out := make(map[string][]Note, len(elems))
	for _, e := range elems {
		id, tag, text, ok := SplitNoteElement(e)
		if !ok {
			continue
		}
		out[id] = append(out[id], Note{Text: text, Tag: tag})
	}
	for _, notes := range out {
		sort.Slice(notes, func(i, j int) bool { // by time, then node: the order people would read them in
			if c := notes[i].Tag.Stamp.Compare(notes[j].Tag.Stamp); c != 0 {
				return c < 0
			}
			return notes[i].Tag.Node < notes[j].Tag.Node
		})
	}
	return out
}
