package bench

import (
	"fmt"
	"strings"
)

// DefaultPrompts returns the two prompts the benchmark uses.
//
//   - "short" measures the typical quick agent call: tiny prompt, short answer.
//   - "long" has a prompt of roughly 800 tokens. It is dominated by prompt
//     processing (prefill), which is the part that is slow on a CPU-only
//     laptop and that most people forget to measure.
//
// Both cap the answer at the same length so generation speed is comparable.
// With quick=true only the short prompt is returned.
func DefaultPrompts(quick bool) []Prompt {
	short := Prompt{
		Name:      "short",
		Text:      "In two sentences, explain what a CRDT is and why it helps when devices are offline.",
		MaxTokens: 96,
	}
	if quick {
		return []Prompt{short}
	}
	return []Prompt{short, {Name: "long", Text: longPrompt(), MaxTokens: 96}}
}

// longPrompt builds a deterministic field-report log plus a question. It is
// generated rather than stored so the text is identical on every machine.
func longPrompt() string {
	var b strings.Builder
	b.WriteString("You are an assistant for field workers. Below is today's site log.\n\n")
	for i := 1; i <= 14; i++ {
		fmt.Fprintf(&b,
			"Entry %d. Site %d, %02d:%02d: pump P-%d is leaking at the inlet flange; priority %s; "+
				"the crew is waiting for a replacement gasket and will inspect the seal again after "+
				"the afternoon shift. Generator fuel is at %d percent and a refuel is scheduled. "+
				"The access road near gate %d was flooded this morning and vehicles were rerouted.\n",
			i, 1+i%7, 6+i%12, (i*7)%60, 10+i, []string{"low", "medium", "high"}[i%3], 20+(i*5)%70, 1+i%4)
	}
	b.WriteString("\nQuestion: list the open issues in at most three short bullet points.")
	return b.String()
}
