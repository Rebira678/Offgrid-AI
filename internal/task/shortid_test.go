package task

import (
	"strings"
	"testing"
)

func TestShortIDHidesTheReplicaOnlyOnTheDay24Shape(t *testing.T) {
	for in, want := range map[string]string{
		"T3.laptop":          "T3",
		"T12.n-1a2b3c4d5e6f": "T12",
		"T3":                 "T3", // plain IDs of older databases
		"T3.":                "T3.",
		"X3.laptop":          "X3.laptop",
		"T.laptop":           "T.laptop",
		"Ta.laptop":          "Ta.laptop",
		"":                   "",
	} {
		if got := ShortID(in); got != want {
			t.Errorf("ShortID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortIDsKeepsFullIDsOnlyWhereTwoWouldLookAlike(t *testing.T) {
	ts := []Task{{ID: "T1.laptop"}, {ID: "T2.laptop"}, {ID: "T2.phone"}, {ID: "T3"}}
	got := strings.Join(ShortIDs(ts), " ")
	if got != "T1 T2.laptop T2.phone T3" {
		t.Fatalf("got %q", got)
	}
	if len(ShortIDs(nil)) != 0 {
		t.Fatal("nil in, something out")
	}
}

// What is shown can be typed back: the short form resolves to the one task.
func TestAShortIDShownResolvesBackToTheTask(t *testing.T) {
	ts := []Task{{ID: "T1.laptop", Title: "pump"}, {ID: "T2.phone", Title: "filter"}}
	for _, tk := range ts {
		got, err := Resolve(ts, ShortID(tk.ID))
		if err != nil || got.ID != tk.ID {
			t.Fatalf("%s: %+v %v", ShortID(tk.ID), got, err)
		}
	}
}
