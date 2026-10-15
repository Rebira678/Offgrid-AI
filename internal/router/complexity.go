// Package router decides what to do with a message the moment it arrives:
// answer it now with the local agent, or defer it to the durable queue.
//
// The decision uses three signals, in this order:
//
//  1. Complexity (this file): does the message ask for judgement, summary over
//     time or advice, which five tools and a small model cannot give?
//  2. Output validity: if the local model was tried and produced nothing valid
//     (or is not there), the message waits instead of failing.
//  3. Connectivity: a hard message is answered by a cloud model only if one
//     exists AND the network is up; otherwise it waits.
package router

import (
	"fmt"
	"regexp"
	"strings"
)

// Complexity is the verdict of the gate on one message.
type Complexity struct {
	Hard    bool
	Score   int
	Signals []string // what was noticed, for showing to a person
}

// Reason is one readable phrase for why the gate said Hard.
func (c Complexity) Reason() string {
	if len(c.Signals) == 0 {
		return "no sign of a request for judgement"
	}
	return "asks for judgement or analysis (" + strings.Join(c.Signals, "; ") + ")"
}

// Threshold is the score at which a message counts as hard.
const Threshold = 3

// The vocabulary. Closed lists on purpose, in the spirit of Day 5: precision
// over coverage. A word is only a signal when it appears as a whole word.
//
//	strong (3 points): alone enough, because nothing in the tool menu does this
//	weak   (2 points): also a common word in plain task titles ("suggest a
//	                   supplier"), so it needs one more hint to count
//	hint   (1 point each, once per kind): a time span, a long message, or a
//	                   second request joined with "and"
var (
	strongTerms = []string{
		"recommend", "recommendation", "recommendations", "advise", "advice",
		"analyze", "analyse", "analysis", "evaluate", "assess", "forecast", "compare",
		"prioritize", "prioritise", "prioritization", "prioritisation",
		"what should", "how should", "how can we", "which should", "should we",
		"why", "root cause", "best way",
		// found by measuring (see docs/day-10.md): open-ended asks the first
		// version missed
		"rundown", "what went wrong", "what to do", "ideas", "how are we doing",
		"difference between", "getting worse", "getting better", "worse than", "better than",
		"compared to", "compared with", "pattern", "patterns", "trend", "trends",
	}
	weakTerms = []string{
		"suggest", "suggestion", "suggestions", "explain", "draft", "plan",
		"think about", "help me decide", "help me understand", "figure out", "any ideas",
	}
	scopeTerms = []string{
		"week", "weeks", "month", "months", "overall", "lately", "across", "incidents", "so far",
	}
	// the whole list rather than one task: "suggest an order for the open tasks"
	wholeListTerms = []string{"open tasks", "all tasks", "the tasks", "every task", "each task", "everything"}
	// a second request: "... and suggest ...", "then recommend ..."
	// writing a document, or picking the best/first/next of several
	writeRe       = regexp.MustCompile(`\b(write|draft|compose|prepare) (me )?(a|an|the) (\w+ )?(report|note|email|message|letter|summary|brief|handover)\b`)
	chooseRe      = regexp.MustCompile(`\bwhich\b.*\b(first|best|worst|next|most|least|needs? attention)\b`)
	conjunctionRe = regexp.MustCompile(`\b(and|then|also|plus) (also |then |please )?(suggest|recommend|prioriti[sz]e|explain|analy[sz]e|advise|draft|plan|assess|evaluate|compare|forecast|decide)\b`)
)

const longMessageWords = 9

// Assess scores a message. It is a handful of string checks and takes
// microseconds: it runs before any model, so it must be cheap and must never
// fail.
func Assess(text string) Complexity {
	clean := normalize(text)
	padded := " " + clean + " "
	var c Complexity
	add := func(points int, signal string) {
		c.Score += points
		c.Signals = append(c.Signals, signal)
	}
	for _, t := range strongTerms {
		if strings.Contains(padded, " "+t+" ") {
			add(3, `"`+t+`"`)
			break // one strong term is already enough; do not stack synonyms
		}
	}
	for _, t := range weakTerms {
		if strings.Contains(padded, " "+t+" ") {
			add(2, `"`+t+`"`)
			break
		}
	}
	for _, t := range scopeTerms {
		if strings.Contains(padded, " "+t+" ") {
			add(1, "a time span: "+t)
			break
		}
	}
	if m := writeRe.FindString(clean); m != "" {
		add(3, fmt.Sprintf("writing a document (%q)", m))
	}
	if m := chooseRe.FindString(clean); m != "" {
		add(3, "choosing the best or first of several")
	}
	for _, t := range wholeListTerms {
		if strings.Contains(padded, " "+t+" ") {
			add(1, "about the whole list: "+t)
			break
		}
	}
	if m := conjunctionRe.FindString(clean); m != "" {
		add(1, fmt.Sprintf("a second request (%q)", m))
	}
	if len(strings.Fields(clean)) >= longMessageWords {
		add(1, "long message")
	}
	c.Hard = c.Score >= Threshold
	return c
}

// normalize lower-cases, turns curly apostrophes straight, drops punctuation
// and collapses spaces, so "What should I prioritize this week?" and
// "what should i prioritize this week" look the same.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("’", "'", "‘", "'").Replace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\'':
			// "week's" -> "weeks": drop the apostrophe so possessives match
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r > 127:
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
