package crdt

import "strings"

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
	out := map[string]map[string]string{}
	for k, e := range m {
		id, field, ok := SplitTaskKey(k)
		if !ok || e.Deleted {
			continue
		}
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		out[id][field] = e.Value
	}
	return out
}
