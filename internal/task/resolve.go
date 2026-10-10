package task

import (
	"strings"
	"unicode"
)

// Resolve finds the one task a free-text reference means.
//
// The model writes a task_ref like "the pump", "T3" or "#3". Resolve is the
// code's answer to "which task is that?", and its main rule is: never guess.
//
//  1. An ID ("T3", "t3", "T-3", "3", "#3") must match an ID exactly. An ID-shaped
//     reference never falls back to a title search.
//  2. Otherwise titles are searched, ignoring case and extra spaces. An exact
//     title match beats a "contains" match, and unfinished tasks are searched
//     before finished ones, so "the pump" means the open pump task, not last
//     month's finished one.
//  3. If exactly one task matches, that is the answer. If several do, the error
//     is an *AmbiguousError listing them. If none, ErrNotFound.
//
// Acting on the wrong task silently is worse than asking again.
func Resolve(tasks []Task, ref string) (Task, error) {
	ref = strings.TrimSpace(ref)
	if id, ok := idShaped(ref); ok {
		for _, t := range tasks {
			if t.ID == id {
				return t, nil
			}
		}
		return Task{}, ErrNotFound
	}

	want := fold(ref)
	if want == "" {
		return Task{}, ErrNotFound
	}
	var active, finished []Task
	for _, t := range tasks {
		if t.Status == StatusDone {
			finished = append(finished, t)
		} else {
			active = append(active, t)
		}
	}
	matchers := []func(title string) bool{
		func(title string) bool { return title == want },
		func(title string) bool { return strings.Contains(title, want) },
	}
	for _, match := range matchers {
		for _, pool := range [][]Task{active, finished} {
			var hits []Task
			for _, t := range pool {
				if match(fold(t.Title)) {
					hits = append(hits, t)
				}
			}
			switch len(hits) {
			case 0:
			case 1:
				return hits[0], nil
			default:
				return Task{}, &AmbiguousError{Ref: ref, Candidates: hits}
			}
		}
	}
	return Task{}, ErrNotFound
}

// idShaped recognises "T3", "t3", "#3", "#T3" and "3", and returns "T3".
func idShaped(ref string) (string, bool) {
	s := strings.TrimPrefix(ref, "#")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "T"), "t")
	if len(s) < len(strings.TrimPrefix(ref, "#")) {
		// Written with a T, people add a separator: "T-12", "T 12", "T_12".
		s = strings.TrimLeft(s, "-_ ")
	}
	if s == "" {
		return "", false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return "T" + s, true
}

// fold lower-cases and collapses runs of whitespace, so "Leaking  PUMP" and
// "leaking pump" compare equal.
func fold(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), unicode.IsSpace), " ")
}
