package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"offgrid/internal/intent"
	"offgrid/internal/llm"
)

// NewReference returns the model CI uses: a deterministic stand-in that answers each case from its
// label, with the faults real small models have, injected on a fixed schedule:
//
//   - every 10th case (starting at the 4th), asked WITHOUT a schema, wraps its JSON in chatty text;
//   - every 10th case (starting at the 7th), asked WITHOUT a schema, answers with broken JSON the first
//     time and correctly on the retry;
//   - every 25th case (starting at the 8th) gets the wrong intent, whatever it is asked.
//
// A request WITH a schema never gets the first two faults, which is what constrained decoding does for
// real. So the reference is not a model of quality: it cannot tell a good prompt from a bad one. What it
// guards is everything around the model: the prompt layout, the schema being sent, the JSON repair, the
// validator and the retry loop. A change that breaks any of them moves these numbers, and the gate fails.
// Model quality is measured by the nightly run against a real model.
func NewReference(cases []Case) llm.Client {
	byText := map[string]int{}
	for i, c := range cases {
		byText[c.Text] = i
	}
	cases = append([]Case(nil), cases...)
	examples := len(intent.ExampleTexts())
	return &llm.Fake{Handler: func(_ context.Context, req llm.Request) (string, error) {
		// Layout: system, (example user, example reply) x N, the worker's message, and on a retry the
		// bad reply and the feedback after that. A prompt that no longer has this layout finds nothing.
		at := 1 + 2*examples
		unknown := cmdText(intent.Command{Intent: intent.Unknown})
		if len(req.Messages) <= at || req.Messages[at].Role != llm.RoleUser {
			return unknown, nil
		}
		i, ok := byText[req.Messages[at].Content]
		if !ok {
			return unknown, nil
		}
		c := cases[i]
		retry := len(req.Messages) > at+1
		constrained := len(req.ResponseSchema) > 0
		cmd := intent.Command{
			Intent: c.Intent, Title: "x", TaskRef: "x", Note: "x",
			Site: c.Site, Priority: c.Priority, Status: c.Status, When: c.When,
		}
		if i%25 == 7 {
			cmd.Intent = intent.Unknown
		}
		text := cmdText(cmd)
		switch {
		case constrained || retry:
			return text, nil
		case i%10 == 3:
			return "Sure! Here is the command: " + text + " Let me know if you need anything else.", nil
		case i%10 == 6:
			return strings.TrimSuffix(text, "}"), nil
		}
		return text, nil
	}}
}

func cmdText(c intent.Command) string {
	b, err := json.Marshal(c)
	if err != nil {
		panic(fmt.Sprint("eval: marshal a command: ", err)) // a struct of strings cannot fail
	}
	return string(b)
}
