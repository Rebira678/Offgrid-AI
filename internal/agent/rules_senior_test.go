package agent_test

import (
	"testing"

	"offgrid/internal/agent"
	"offgrid/internal/intent"
)

func BenchmarkRulesMatch(b *testing.B) {
	rules := agent.DefaultRules()
	inputs := []string{
		"new task: replace the water filter at site 4",
		"mark T3 as done",
		"list open tasks",
		"summary of today please",
		"what's the weather tomorrow", // negative case / fallback
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		text := inputs[i%len(inputs)]
		_, _, _ = rules.Match(text)
	}
}

func TestPolitenessCleaningCombinations(t *testing.T) {
	rules := agent.DefaultRules()
	variants := []string{
		"could you please mark T1 as done",
		"can you please mark T1 as done please",
		"hey please mark T1 as done",
		"i need to mark T1 as done",
		"ok mark T1 as done please",
	}

	for _, text := range variants {
		cmd, rule, ok := rules.Match(text)
		if !ok {
			t.Errorf("failed to match polite variant: %q", text)
			continue
		}
		if rule != "update-status" || cmd.Intent != intent.UpdateTask || cmd.TaskRef != "T1" || cmd.Status != "done" {
			t.Errorf("polite variant %q gave unexpected command: rule=%s, cmd=%+v", text, rule, cmd)
		}
	}
}

func TestEmptyAndWhitespaceRequests(t *testing.T) {
	rules := agent.DefaultRules()
	blanks := []string{
		"",
		"   ",
		"\t\n\r",
		"   please   ",
		"could you",
	}

	for _, text := range blanks {
		_, _, ok := rules.Match(text)
		if ok {
			t.Errorf("expected blank/polite-only input %q to not match any rule", text)
		}
	}
}
