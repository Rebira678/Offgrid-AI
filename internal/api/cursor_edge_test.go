package api

import (
	"encoding/base64"
	"net/url"
	"testing"

	"offgrid/internal/task"
)

func TestCursorEdgeCases(t *testing.T) {
	r := newRig(t, nil)
	r.mk("task 1", "site1", task.PriorityLow)
	r.mk("task 2", "site2", task.PriorityHigh)

	badCursors := []struct {
		name string
		raw  string
	}{
		{"not base64", "!!!not-valid-base64!!!"},
		{"no dot separator", base64.RawURLEncoding.EncodeToString([]byte("1234567890"))},
		{"empty id part", base64.RawURLEncoding.EncodeToString([]byte("1234567890."))},
		{"empty timestamp part", base64.RawURLEncoding.EncodeToString([]byte(".id123"))},
		{"non-numeric timestamp", base64.RawURLEncoding.EncodeToString([]byte("notanumber.id123"))},
		{"overflow timestamp", base64.RawURLEncoding.EncodeToString([]byte("999999999999999999999999999999999999999999.id123"))},
		{"standard padding with equal signs", "MTIzNC5pZA=="},
	}

	for _, bc := range badCursors {
		t.Run(bc.name, func(t *testing.T) {
			path := "/v1/tasks?cursor=" + url.QueryEscape(bc.raw)
			rp := r.do("GET", path, nil, nil)
			if rp.code != 400 {
				t.Fatalf("expected 400 for cursor %q, got %d: %s", bc.raw, rp.code, rp.body)
			}
		})
	}

	// Boundary limits
	badLimits := []string{"0", "-1", "-100", "501", "99999", "abc", "1.5"}
	for _, bl := range badLimits {
		t.Run("limit_"+bl, func(t *testing.T) {
			rp := r.do("GET", "/v1/tasks?limit="+bl, nil, nil)
			if rp.code != 400 {
				t.Fatalf("expected 400 for limit %q, got %d: %s", bl, rp.code, rp.body)
			}
		})
	}

	// Status and priority filter validation
	badFilters := []string{
		"status=OPEN",        // case sensitive (must be open)
		"status=DONE",        // case sensitive (must be done)
		"status=in-progress", // invalid delimiter
		"priority=HIGH",      // case sensitive
		"priority=urgent",    // not a known priority
	}
	for _, bf := range badFilters {
		t.Run("filter_"+bf, func(t *testing.T) {
			rp := r.do("GET", "/v1/tasks?"+bf, nil, nil)
			if rp.code != 400 {
				t.Fatalf("expected 400 for bad filter %q, got %d: %s", bf, rp.code, rp.body)
			}
		})
	}

	// Empty filter values should be ignored gracefully
	rp := r.do("GET", "/v1/tasks?status=&priority=&site=", nil, nil)
	if rp.code != 200 {
		t.Fatalf("expected 200 for empty filters, got %d: %s", rp.code, rp.body)
	}

	// Sequential limit=1 step through all tasks
	var collected []string
	cursor := ""
	for i := 0; i < 5; i++ {
		path := "/v1/tasks?limit=1"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var l taskList
		rp := r.do("GET", path, nil, nil)
		if rp.code != 200 {
			t.Fatalf("step %d failed with code %d: %s", i, rp.code, rp.body)
		}
		rp.into(t, &l)
		if len(l.Tasks) > 0 {
			collected = append(collected, l.Tasks[0].Title)
		}
		if l.NextCursor == nil {
			break
		}
		cursor = *l.NextCursor
	}
	if len(collected) != 2 || collected[0] != "task 1" || collected[1] != "task 2" {
		t.Fatalf("unexpected sequential paging collection: %v", collected)
	}
}
