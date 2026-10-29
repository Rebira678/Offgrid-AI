package sim

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// TestParseActionExhaustiveEdgeCases checks strict parsing validation
// against all invalid, truncated, and edge-case syntax representations.
func TestParseActionExhaustiveEdgeCases(t *testing.T) {
	for _, bad := range []string{
		"",
		"   ",
		"create",
		"create r0",
		"create r0 n=",
		"create r0 n=-1",
		"create r0 n=abc",
		"create r0 n=1.23",
		"create r0 n=999999999999999999999999999999999999999999",
		"create r-1 n=5",
		"create rX n=5",
		"create rx n=5",
		"create 0 n=5",
		"create r0 n=5 extra",
		"dance r0 n=1",
		"explode r1 n=2",
		"sync",
		"sync r0",
		"sync r0 n=1",
		"sync r0-r0 n=1",  // self-sync is invalid
		"sync r0-r-1 n=1", // negative replica
		"sync r-1-r0 n=1", // negative replica
		"sync r0-rx n=1",  // non-numeric
		"sync rx-r0 n=1",  // non-numeric
		"sync r0-r1",      // missing n=
		"sync r0-r1 n=-5", // negative n
		"sync r0-r1 n=5 extra",
		"partition r0 n=1",    // partition must have pair
		"partition r0-r0 n=1", // self partition invalid
		"heal r0 n=1",         // heal must have pair
		"heal r0-r0 n=1",      // self heal invalid
		"wait",                // missing n
		"wait r0 n=1",         // wait takes no replica
		"wait n=-1",           // negative wait
		"wait n=1 extra",      // extra arg
	} {
		if a, err := ParseAction(bad); err == nil {
			t.Errorf("ParseAction(%q) succeeded as %+v, want error", bad, a)
		}
	}
}

// TestActionStringFormattingRoundtripsAllKinds checks that every Action kind
// produces expected string representation and round-trips through ParseAction.
func TestActionStringFormattingRoundtripsAllKinds(t *testing.T) {
	kinds := []Kind{
		Create, Update, Note, Delete, Suggest, Resolve,
		Skew, Crash, Partition, Heal, Sync, Wait,
	}

	for _, k := range kinds {
		a := Action{Kind: k, A: 1, B: 2, N: 42}
		str := a.String()
		switch k {
		case Partition, Heal, Sync:
			if !strings.Contains(str, "r1-r2") || !strings.Contains(str, "n=42") {
				t.Errorf("%s String() = %q, want pair and n=42", k, str)
			}
		case Wait:
			if str != "wait n=42" {
				t.Errorf("wait String() = %q, want 'wait n=42'", str)
			}
		default:
			if !strings.Contains(str, "r1") || !strings.Contains(str, "n=42") {
				t.Errorf("%s String() = %q, want r1 and n=42", k, str)
			}
		}

		parsed, err := ParseAction(str)
		if err != nil {
			t.Fatalf("ParseAction(%q) failed: %v", str, err)
		}
		if parsed.Kind != a.Kind || parsed.N != a.N {
			t.Fatalf("roundtrip mismatch: got %+v, want %+v", parsed, a)
		}
		if k == Partition || k == Heal || k == Sync {
			if parsed.A != a.A || parsed.B != a.B {
				t.Fatalf("roundtrip pair mismatch: got %+v, want %+v", parsed, a)
			}
		} else if k != Wait {
			if parsed.A != a.A {
				t.Fatalf("roundtrip replica mismatch: got %+v, want %+v", parsed, a)
			}
		}
	}
}

// TestConfigWithDefaultsBoundaries tests that Config clamps non-positive
// or out-of-boundary values to valid defaults.
func TestConfigWithDefaultsBoundaries(t *testing.T) {
	zero := Config{}.withDefaults()
	if zero.Replicas != 3 {
		t.Errorf("Replicas = %d, want 3", zero.Replicas)
	}
	if zero.Steps != 80 {
		t.Errorf("Steps = %d, want 80", zero.Steps)
	}
	if zero.Dir == "" {
		t.Errorf("Dir is empty, want non-empty path")
	}

	negative := Config{Replicas: -5, Steps: -20}.withDefaults()
	if negative.Replicas != 3 || negative.Steps != 80 {
		t.Errorf("negative config not clamped: %+v", negative)
	}

	customDir := t.TempDir()
	custom := Config{Replicas: 5, Steps: 120, Dir: customDir}.withDefaults()
	if custom.Replicas != 5 || custom.Steps != 120 || custom.Dir != customDir {
		t.Errorf("custom config altered: %+v", custom)
	}
}

// TestStatsAdditionArithmetic verifies that Stats.add sums every counter.
func TestStatsAdditionArithmetic(t *testing.T) {
	s1 := Stats{
		Edits: 1, Syncs: 2, Partitions: 3, Blocked: 4, Skews: 5, Crashes: 6,
		Dropped: 7, Duplicated: 8, Delayed: 9, Truncated: 10, DriftRefusals: 11,
		Rebuilds: 12, Reversed: 13, Gapped: 14, GapRefused: 15,
	}
	s2 := Stats{
		Edits: 10, Syncs: 20, Partitions: 30, Blocked: 40, Skews: 50, Crashes: 60,
		Dropped: 70, Duplicated: 80, Delayed: 90, Truncated: 100, DriftRefusals: 110,
		Rebuilds: 120, Reversed: 130, Gapped: 140, GapRefused: 150,
	}

	s1.add(s2)

	if s1.Edits != 11 || s1.Syncs != 22 || s1.Partitions != 33 || s1.Blocked != 44 ||
		s1.Skews != 55 || s1.Crashes != 66 || s1.Dropped != 77 || s1.Duplicated != 88 ||
		s1.Delayed != 99 || s1.Truncated != 110 || s1.DriftRefusals != 121 ||
		s1.Rebuilds != 132 || s1.Reversed != 143 || s1.Gapped != 154 || s1.GapRefused != 165 {
		t.Errorf("Stats.add failed to add all fields correctly: %+v", s1)
	}
}

// TestFailureErrorString checks the Error() string formatting.
func TestFailureErrorString(t *testing.T) {
	f := &Failure{Class: "invariance", Msg: "replicas diverged at step 4"}
	want := "invariance: replicas diverged at step 4"
	if f.Error() != want {
		t.Errorf("Failure.Error() = %q, want %q", f.Error(), want)
	}
}

// TestOracleRejectsCorruptJSONInLog verifies that the oracle catches
// non-JSON or damaged payloads in the log.
func TestOracleRejectsCorruptJSONInLog(t *testing.T) {
	ctx := context.Background()
	clk := &tclock{epoch}
	s := rep(t, "node-corrupt", clk)

	// An op with completely broken JSON payload
	corruptOp := []sqlstore.Op{
		{
			NodeID:  "node-corrupt",
			Seq:     1,
			TS:      epoch,
			Kind:    sqlstore.KindTaskCreated,
			Payload: []byte(`{this is not valid json`),
		},
	}

	f := checkAgainstTheLog(ctx, s, corruptOp)
	if f == nil || f.Class != "oracle" {
		t.Fatalf("expected Failure with Class='oracle', got %+v", f)
	}
}

// TestOracleHandlesEmptyLogCleanly verifies checkAgainstTheLog with an empty store.
func TestOracleHandlesEmptyLogCleanly(t *testing.T) {
	ctx := context.Background()
	clk := &tclock{epoch}
	s := rep(t, "node-empty", clk)

	f := checkAgainstTheLog(ctx, s, nil)
	if f != nil {
		t.Fatalf("empty log produced failure: %v", f)
	}
}

// TestOracleSuggestionResolvedMultipleTimes checks that the oracle adheres to
// the first resolution in fold order.
func TestOracleSuggestionResolvedMultipleTimes(t *testing.T) {
	ctx := context.Background()
	clk := &tclock{epoch}
	a, b := rep(t, "a", clk), rep(t, "b", clk)

	tk, _, err := a.Create(ctx, task.NewTask{Title: "safety inspect", Priority: task.PriorityLow})
	if err != nil {
		t.Fatal(err)
	}
	pullAll(t, b, a)

	hi := task.PriorityHigh
	sug, _, err := a.Suggest(ctx, task.Draft{Task: tk.ID, Priority: &hi, By: "ai-model"})
	if err != nil {
		t.Fatal(err)
	}
	pullAll(t, b, a)

	// A accepts the suggestion
	clk.t = clk.t.Add(time.Minute)
	if _, err := a.Resolve(ctx, sug.ID, true); err != nil {
		t.Fatal(err)
	}

	// B dismisses the suggestion
	clk.t = clk.t.Add(time.Minute)
	if _, err := b.Resolve(ctx, sug.ID, false); err != nil {
		t.Fatal(err)
	}

	pullAll(t, a, b)
	pullAll(t, b, a)

	ops := opsOf(t, a)
	// The oracle must verify successfully matching A's first resolution
	if f := checkAgainstTheLog(ctx, a, ops); f != nil {
		t.Fatalf("oracle check failed: %v", f)
	}
}

// TestManySeedRangeAndConcurrencyDeterminism tests that Many executes
// across arbitrary seed ranges and produces identical results regardless of worker count.
func TestManySeedRangeAndConcurrencyDeterminism(t *testing.T) {
	cfg := Config{Replicas: 3, Steps: 20}
	sum1 := Many(50, 10, 1, cfg)
	sum2 := Many(50, 10, 4, cfg)

	if sum1.From != 50 || sum1.To != 59 || sum1.Runs != 10 {
		t.Errorf("sum1 bounds wrong: %+v", sum1)
	}
	if sum2.From != 50 || sum2.To != 59 || sum2.Runs != 10 {
		t.Errorf("sum2 bounds wrong: %+v", sum2)
	}
	if len(sum1.Failed) != len(sum2.Failed) {
		t.Errorf("failed count differs: %d vs %d", len(sum1.Failed), len(sum2.Failed))
	}
	if sum1.Ops != sum2.Ops {
		t.Errorf("ops differ between 1 and 4 workers: %d vs %d", sum1.Ops, sum2.Ops)
	}
	if !reflect.DeepEqual(sum1.Total, sum2.Total) {
		t.Errorf("stats differ between 1 and 4 workers: %+v vs %+v", sum1.Total, sum2.Total)
	}
}

// TestShrinkNilFailureReturnsSelf verifies Shrink returns when Failure is nil.
func TestShrinkNilFailureReturnsSelf(t *testing.T) {
	r := Result{Seed: 10, Failure: nil, Actions: []Action{{Kind: Wait, N: 1}}}
	shrunk := Shrink(r, Config{})
	if shrunk.Failure != nil || len(shrunk.Actions) != 1 {
		t.Fatalf("Shrink of non-failure modified result: %+v", shrunk)
	}
}

// TestCustomReplicasAndLargeStepGeneration tests generating a wide schedule.
func TestCustomReplicasAndLargeStepGeneration(t *testing.T) {
	cfg := Config{Replicas: 6, Steps: 200}
	acts := Generate(42, cfg)
	if len(acts) != 200 {
		t.Fatalf("len(acts) = %d, want 200", len(acts))
	}
	for i, a := range acts {
		if a.A < 0 || a.A >= 6 {
			t.Fatalf("action %d: replica A out of bounds: %v", i, a)
		}
		if (a.Kind == Sync || a.Kind == Partition || a.Kind == Heal) && (a.B < 0 || a.B >= 6 || a.A == a.B) {
			t.Fatalf("action %d: replica pair invalid: %v", i, a)
		}
	}
}
