package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"offgrid/internal/intent"
	"offgrid/internal/llm"
)

// ---- the dataset ----------------------------------------------------------

func TestDatasetIsWellFormed(t *testing.T) {
	cases, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 30 {
		t.Errorf("have %d cases, want 30", len(cases))
	}

	ids := map[string]bool{}
	texts := map[string]bool{}
	perIntent := map[intent.Intent]int{}
	for _, c := range cases {
		if c.ID == "" || ids[c.ID] {
			t.Errorf("case id %q is empty or duplicated", c.ID)
		}
		ids[c.ID] = true
		key := strings.ToLower(strings.TrimSpace(c.Text))
		if key == "" || texts[key] {
			t.Errorf("case %s has empty or duplicated text", c.ID)
		}
		texts[key] = true
		perIntent[c.Intent]++

		// The expected values must themselves be legal, or no model could ever pass.
		check := func(field, v string, allowed []string) {
			if v == "" {
				return
			}
			for _, a := range allowed {
				if a == v {
					return
				}
			}
			t.Errorf("case %s: expected %s %q is not an allowed value", c.ID, field, v)
		}
		check("priority", c.Priority, intent.Priorities())
		check("status", c.Status, intent.Statuses())
		check("when", c.When, intent.Whens())
	}
	for _, i := range intent.Intents() {
		if perIntent[i] < 3 {
			t.Errorf("intent %s has only %d cases; every intent needs at least 3", i, perIntent[i])
		}
	}
}

// If a worked example in the prompt also appears in the test set, the model
// is being tested on something it was just shown the answer to, and the score
// means nothing.
func TestNoPromptExampleLeaksIntoTheDataset(t *testing.T) {
	cases, err := LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range cases {
		seen[strings.ToLower(strings.TrimSpace(c.Text))] = true
	}
	for _, ex := range intent.ExampleTexts() {
		if seen[strings.ToLower(strings.TrimSpace(ex))] {
			t.Errorf("prompt example %q is also an evaluation case", ex)
		}
	}
}

// ---- statistics -----------------------------------------------------------

func TestWilsonInterval(t *testing.T) {
	cases := []struct {
		k, n   int
		lo, hi float64
	}{
		{27, 30, 0.744, 0.965},
		{15, 30, 0.332, 0.668},
		{0, 30, 0.000, 0.114},
		{30, 30, 0.886, 1.000},
	}
	for _, tc := range cases {
		lo, hi := Proportion{tc.k, tc.n}.Wilson()
		if math.Abs(lo-tc.lo) > 0.002 || math.Abs(hi-tc.hi) > 0.002 {
			t.Errorf("Wilson(%d/%d) = [%.3f, %.3f], want about [%.3f, %.3f]", tc.k, tc.n, lo, hi, tc.lo, tc.hi)
		}
	}
	if lo, hi := (Proportion{}).Wilson(); lo != 0 || hi != 0 {
		t.Errorf("empty proportion = [%v, %v], want [0, 0]", lo, hi)
	}
}

func TestProportionString(t *testing.T) {
	if got := (Proportion{27, 30}).String(); got != "27/30 = 90% [74-97]" {
		t.Errorf("got %q", got)
	}
	if got := (Proportion{}).String(); got != "n/a" {
		t.Errorf("got %q, want n/a", got)
	}
}

// With 30 cases, 25/30 and 27/30 must NOT be separable. If this ever fails,
// the report's warning about overlapping intervals has stopped being true.
func TestSmallDifferencesAreNotSignificant(t *testing.T) {
	_, hiLow := Proportion{25, 30}.Wilson()
	loHigh, _ := Proportion{27, 30}.Wilson()
	if loHigh > hiLow {
		t.Error("25/30 and 27/30 should have overlapping intervals")
	}
}

// ---- running ---------------------------------------------------------------

func cmdJSON(c intent.Command) string {
	b, _ := json.Marshal(c)
	return string(b)
}

// oracle answers every case correctly by looking up the expected answer.
func oracle(t *testing.T) *llm.Fake {
	t.Helper()
	cases, _ := LoadCases()
	byText := map[string]Case{}
	for _, c := range cases {
		byText[c.Text] = c
	}
	return &llm.Fake{Handler: func(_ context.Context, req llm.Request) (string, error) {
		text := req.Messages[len(req.Messages)-1].Content
		c, ok := byText[text]
		if !ok {
			return cmdJSON(intent.Command{Intent: intent.Unknown}), nil
		}
		return cmdJSON(intent.Command{
			Intent: c.Intent, Title: "x", TaskRef: "x", Note: "x",
			Site: c.Site, Priority: c.Priority, Status: c.Status, When: c.When,
		}), nil
	}}
}

func TestPerfectModelScoresFull(t *testing.T) {
	cases, _ := LoadCases()
	sum, err := Run(context.Background(), Config{Client: oracle(t), Model: "m", UseSchema: true}, cases)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Cases != 30 || sum.CleanFirstTry != 30 || sum.ValidFirstTry != 30 || sum.IntentCorrect != 30 {
		t.Errorf("summary = %+v", sum)
	}
	if sum.ArgsChecked == 0 || sum.ArgsCorrect != sum.ArgsChecked {
		t.Errorf("args %d/%d, want all correct", sum.ArgsCorrect, sum.ArgsChecked)
	}
	if sum.Rescued != 0 || sum.Errors != 0 || sum.AvgAttempts() != 1 {
		t.Errorf("rescued %d, errors %d, avg attempts %v", sum.Rescued, sum.Errors, sum.AvgAttempts())
	}
}

func TestLimitRunsOnlyTheFirstCases(t *testing.T) {
	cases, _ := LoadCases()
	f := oracle(t)
	sum, err := Run(context.Background(), Config{Client: f, Model: "m", Limit: 5}, cases)
	if err != nil || sum.Cases != 5 || len(f.Calls()) != 5 {
		t.Errorf("cases %d, calls %d, err %v; want 5, 5, nil", sum.Cases, len(f.Calls()), err)
	}
}

func TestWarmupIsNotCounted(t *testing.T) {
	cases, _ := LoadCases()
	f := oracle(t)
	sum, err := Run(context.Background(), Config{Client: f, Model: "m", Limit: 3, Warmup: true}, cases)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Cases != 3 || len(f.Calls()) != 4 {
		t.Errorf("cases %d, calls %d; want 3 counted cases and 4 calls (1 warm-up)", sum.Cases, len(f.Calls()))
	}
}

// A model that is wrong about one intent, malformed once and fixed on retry,
// and hopeless on another case: every counter must land in the right place.
func TestCountersForMixedBehaviour(t *testing.T) {
	cases := []Case{
		{ID: "a", Text: "msg-a", Intent: intent.ListTasks, Status: "open"},
		{ID: "b", Text: "msg-b", Intent: intent.CreateTask},
		{ID: "c", Text: "msg-c", Intent: intent.Unknown},
		{ID: "d", Text: "msg-d", Intent: intent.ListTasks},
	}
	f := &llm.Fake{Handler: func(_ context.Context, req llm.Request) (string, error) {
		// On a retry the LAST message is the feedback text, so look for the
		// worker's original message instead.
		var orig string
		for _, m := range req.Messages {
			if strings.HasPrefix(m.Content, "msg-") {
				orig = m.Content
			}
		}
		isRetry := strings.HasPrefix(req.Messages[len(req.Messages)-1].Content, "Your reply was rejected")

		switch orig {
		case "msg-a": // right intent, wrong argument (status done instead of open)
			return cmdJSON(intent.Command{Intent: intent.ListTasks, Status: "done"}), nil
		case "msg-c": // wrong intent
			return cmdJSON(intent.Command{Intent: intent.ListTasks}), nil
		case "msg-d": // never produces anything usable
			return "I do not know", nil
		default: // msg-b: fenced and with an empty title at first, fixed on the retry
			if !isRetry {
				return "```json\n" + cmdJSON(intent.Command{Intent: intent.CreateTask}) + "\n```", nil
			}
			return cmdJSON(intent.Command{Intent: intent.CreateTask, Title: "Fix pump"}), nil
		}
	}}

	sum, err := Run(context.Background(), Config{Client: f, Model: "m", UseSchema: true}, cases)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]int{
		"Cases": 4, "CleanFirstTry": 2, "ValidFirstTry": 2, "Succeeded": 3,
		"IntentCorrect": 2, "ArgsChecked": 1, "ArgsCorrect": 0, "Rescued": 1, "Repaired": 1, "Errors": 0,
	}
	got := map[string]int{
		"Cases": sum.Cases, "CleanFirstTry": sum.CleanFirstTry, "ValidFirstTry": sum.ValidFirstTry,
		"Succeeded": sum.Succeeded, "IntentCorrect": sum.IntentCorrect, "ArgsChecked": sum.ArgsChecked,
		"ArgsCorrect": sum.ArgsCorrect, "Rescued": sum.Rescued, "Repaired": sum.Repaired, "Errors": sum.Errors,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %d, want %d", k, got[k], w)
		}
	}
	// a: 1 attempt, b: 2, c: 1, d: 3 (the default limit) = 7
	if sum.TotalAttempts != 7 {
		t.Errorf("TotalAttempts = %d, want 7", sum.TotalAttempts)
	}
	d := sum.Results[3]
	if d.Succeeded || d.Error == "" || d.Got != "" {
		t.Errorf("case d = %+v, want a recorded failure with no intent", d)
	}
}

func TestBackendOutageStopsTheRun(t *testing.T) {
	cases, _ := LoadCases()
	f := &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) {
		return "", llm.ErrUnavailable
	}}
	sum, err := Run(context.Background(), Config{Client: f, Model: "m"}, cases)
	if !errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	if len(f.Calls()) != 1 || sum.Cases != 0 {
		t.Errorf("made %d calls and counted %d cases; an outage must stop the run at once", len(f.Calls()), sum.Cases)
	}
}

func TestOneTimeoutIsCountedNotFatal(t *testing.T) {
	cases := []Case{
		{ID: "a", Text: "slow", Intent: intent.Unknown},
		{ID: "b", Text: "fast", Intent: intent.Unknown},
	}
	f := &llm.Fake{Handler: func(_ context.Context, req llm.Request) (string, error) {
		if req.Messages[len(req.Messages)-1].Content == "slow" {
			return "", context.DeadlineExceeded
		}
		return cmdJSON(intent.Command{Intent: intent.Unknown}), nil
	}}
	sum, err := Run(context.Background(), Config{Client: f, Model: "m"}, cases)
	if err != nil {
		t.Fatalf("a single timeout should not abort the run: %v", err)
	}
	if sum.Errors != 1 || sum.Cases != 2 || sum.IntentCorrect != 1 {
		t.Errorf("summary = %+v", sum)
	}
}

func TestCancellationReturnsPartialResults(t *testing.T) {
	cases, _ := LoadCases()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	f := &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) {
		calls++
		if calls == 3 {
			cancel() // the user presses Ctrl-C during the third case
		}
		return cmdJSON(intent.Command{Intent: intent.Unknown}), nil
	}}
	sum, err := Run(ctx, Config{Client: f, Model: "m"}, cases)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if sum.Cases != 2 {
		t.Errorf("kept %d finished cases, want 2", sum.Cases)
	}
}

func TestSetupLabels(t *testing.T) {
	cases := map[string]Summary{
		"prompt only":                  {UseSchema: false, MaxAttempts: 1},
		"+ schema":                     {UseSchema: true, MaxAttempts: 1},
		"+ schema + retries (up to 3)": {UseSchema: true, MaxAttempts: 3},
		"prompt + retries (up to 2)":   {UseSchema: false, MaxAttempts: 2},
	}
	for want, s := range cases {
		if got := s.Setup(); got != want {
			t.Errorf("Setup() = %q, want %q", got, want)
		}
	}
}

// ---- reports ---------------------------------------------------------------

func TestReports(t *testing.T) {
	cases, _ := LoadCases()
	good, _ := Run(context.Background(), Config{Client: oracle(t), Model: "good:3b", UseSchema: true, MaxAttempts: 3}, cases)
	bad, _ := Run(context.Background(), Config{
		Client: &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) { return "nope", nil }},
		Model:  "bad:1b", MaxAttempts: 1, Limit: 4,
	}, cases)

	meta := Meta{Date: "2026-10-07", OllamaVersion: "0.9.9"}
	var md bytes.Buffer
	if err := WriteMarkdown(&md, meta, []Summary{good, bad}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| good:3b | + schema + retries (up to 3) | 30/30 = 100%", "| bad:1b | prompt only | 0/4 = 0%",
		"95% confidence interval", "### bad:1b, prompt only", "no valid reply after 1 attempt(s)", "0.9.9",
	} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown is missing %q:\n%s", want, md.String())
		}
	}
	if strings.Contains(md.String(), "### good:3b") {
		t.Error("a run with no misses should not get a misses section")
	}

	var js bytes.Buffer
	if err := WriteJSON(&js, meta, []Summary{good}); err != nil {
		t.Fatal(err)
	}
	var back struct {
		Runs []Summary `json:"runs"`
	}
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || len(back.Runs) != 1 || len(back.Runs[0].Results) != 30 {
		t.Errorf("JSON did not round-trip: %v", err)
	}
}

// Guard against the dataset and the validator disagreeing: a correct answer to
// every case, built only from the expectations, must itself pass validation.
func TestEveryCaseIsAnswerable(t *testing.T) {
	cases, _ := LoadCases()
	for _, c := range cases {
		cmd := intent.Command{Intent: c.Intent, Title: "x", TaskRef: "x", Note: "x",
			Site: c.Site, Priority: c.Priority, Status: c.Status, When: c.When}
		if err := intent.Validate(&cmd); err != nil {
			t.Errorf("case %s cannot be answered validly: %v", c.ID, err)
		}
	}
}
