package agent

import (
	"regexp"
	"strings"

	"offgrid/internal/intent"
)

// Tier 0: rules.
//
// A rule recognises one plain way of saying something and turns it into a
// Command in microseconds, with no model. The design principle is
// PRECISION OVER COVERAGE:
//
//   - A message the rules miss costs a few seconds (the model handles it).
//   - A message the rules MISREAD costs a wrong action, silently.
//
// So a rule fires only when it understands every word of the message. The
// "closed vocabulary" rules (list, summary) refuse the whole message if a
// single word is unfamiliar, so "show tasks NOT done" falls through to the
// model instead of being read as "show tasks done". The "frame" rules (create,
// note, update) need an explicit sentence shape such as "note on T3: text".
//
// A rule never has to be right about everything. It only has to be right when
// it speaks, and silent otherwise.

// Rule is one named matcher.
type Rule struct {
	Name  string
	Match func(clean string) (intent.Command, bool)
}

// Rules is an ordered list; the first rule that matches wins.
type Rules []Rule

// Match cleans text and returns the first matching rule's command.
func (rs Rules) Match(text string) (cmd intent.Command, rule string, ok bool) {
	clean := cleanText(text)
	if clean == "" {
		return intent.Command{}, "", false
	}
	for _, r := range rs {
		if c, ok := r.Match(clean); ok {
			return c, r.Name, true
		}
	}
	return intent.Command{}, "", false
}

// DefaultRules is the Tier 0 rule set. Order matters: the more specific
// frames come before the closed-vocabulary rules.
func DefaultRules() Rules {
	return Rules{
		{"summary-what-happened", matchWhatHappened},
		{"summary", matchSummary},
		{"add-note", matchNote},
		{"create-task", matchCreate},
		{"list", matchList}, // before the update frames: "what is done" is a question, not a status change
		{"update-status", matchStatusChange},
		{"update-is-status", matchIsStatus},
		{"update-verb", matchVerbChange},
		{"update-priority-of", matchPriorityOf},
		{"update-priority", matchPriorityChange},
	}
}

// ---- cleaning ---------------------------------------------------------------

var politePrefixes = []string{
	"please ", "pls ", "hey ", "ok ", "okay ", "can you ", "could you ", "would you ",
	"can u ", "i want to ", "i need to ", "i would like to ", "id like to ", "i'd like to ",
}

// cleanText trims, collapses spaces, and drops trailing punctuation and
// politeness words. It keeps the original case: titles and notes must reach the
// store as the worker wrote them.
func cleanText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for changed := true; changed; {
		changed = false
		s = strings.TrimRight(s, " .!?")
		low := strings.ToLower(s)
		for _, p := range politePrefixes {
			if strings.HasPrefix(low, p) {
				s = s[len(p):]
				changed = true
				break
			}
		}
		if strings.HasSuffix(strings.ToLower(s), " please") {
			s = s[:len(s)-len(" please")]
			changed = true
		}
	}
	return s
}

// ---- summary ----------------------------------------------------------------

var whatHappenedRe = regexp.MustCompile(`(?i)^what(?:'s| is)? (?:happened|did we do|got done|did we get done|have we done) (today|yesterday|this week)$`)

func matchWhatHappened(s string) (intent.Command, bool) {
	m := whatHappenedRe.FindStringSubmatch(s)
	if m == nil {
		return intent.Command{}, false
	}
	return intent.Command{Intent: intent.DailySummary, When: whenWord(m[1])}, true
}

var summaryAnchors = set("summary", "recap", "report", "summarise", "summarize")
var summaryFiller = set("give", "me", "show", "get", "send", "the", "a", "my", "our", "daily",
	"end", "of", "day", "eod", "for", "of", "full", "quick", "write", "make", "generate")

func matchSummary(s string) (intent.Command, bool) {
	toks := tokens(s)
	anchored := false
	var when string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case summaryAnchors[t]:
			anchored = true
		case t == "weekly":
			if when != "" && when != "this_week" {
				return intent.Command{}, false
			}
			when = "this_week"
		case isWhen(t):
			w := whenWord(t)
			if when != "" && when != w {
				return intent.Command{}, false
			}
			when = w
		case summaryFiller[t]:
		default:
			return intent.Command{}, false // a word we do not understand: stay silent
		}
	}
	if !anchored {
		return intent.Command{}, false
	}
	if when == "" {
		// "daily" and "end of day" already say which day. (The registry would
		// default to today anyway; this keeps the proposed command explicit.)
		joined := " " + strings.Join(toks, " ") + " "
		if strings.Contains(joined, " eod ") || strings.Contains(joined, " daily ") || strings.Contains(joined, " end of day ") {
			when = "today"
		}
	}
	return intent.Command{Intent: intent.DailySummary, When: when}, true
}

// ---- list -------------------------------------------------------------------

var listStarters = set("list", "show", "display", "what", "whats", "which", "give", "get", "see", "view")
var listNouns = set("tasks", "task", "tickets", "ticket", "jobs", "job")
var listFiller = set("me", "all", "the", "my", "our", "are", "is", "there", "a", "any", "of",
	"still", "to", "do", "that", "we", "have", "i", "from", "for", "at", "on", "in", "with",
	"created", "added", "raised", "logged", "got", "has", "been", "and", "work", "priority")
var listAnchors = set("left", "remaining", "outstanding", "open", "done", "completed", "finished", "closed", "in_progress")
var statusWords = map[string]string{
	"open": "open", "in_progress": "in_progress",
	"done": "done", "completed": "done", "finished": "done", "closed": "done",
}

func matchList(s string) (intent.Command, bool) {
	low := strings.ToLower(strings.NewReplacer(",", " ", ";", " ", "'", "", "’", "").Replace(s))
	low = strings.NewReplacer("in progress", "in_progress", "this week", "this_week").Replace(low)
	toks := strings.Fields(low)
	if len(toks) < 2 || !listStarters[toks[0]] {
		return intent.Command{}, false
	}
	cmd := intent.Command{Intent: intent.ListTasks}
	anchored := false
	// setOnce fills a slot, and gives up if the message names two values.
	setOnce := func(slot *string, v string) bool {
		if *slot != "" && *slot != v {
			return false
		}
		*slot = v
		return true
	}
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		switch {
		case listNouns[t]:
			anchored = true
		case statusWords[t] != "":
			anchored = true
			if !setOnce(&cmd.Status, statusWords[t]) {
				return intent.Command{}, false
			}
		case t == "low" || t == "medium" || t == "high":
			if !setOnce(&cmd.Priority, t) {
				return intent.Command{}, false
			}
		case isWhen(t):
			if !setOnce(&cmd.When, whenWord(t)) {
				return intent.Command{}, false
			}
		case t == "site" || t == "sites":
			if i+1 >= len(toks) || !validSiteCode(toks[i+1]) {
				return intent.Command{}, false
			}
			i++
			if !setOnce(&cmd.Site, toks[i]) {
				return intent.Command{}, false
			}
		case listAnchors[t]:
			anchored = true
		case listFiller[t]:
		default:
			return intent.Command{}, false // unknown word, for example "not" or "except"
		}
	}
	if !anchored {
		return intent.Command{}, false
	}
	return cmd, true
}

// ---- create -----------------------------------------------------------------

var createRe = regexp.MustCompile(`(?i)^(?:new|create|add|log|raise|make)\s+(?:(?:a|an|new)\s+)*(?:task|ticket|job)(?:\s*:\s*|\s+(?:to|for|about)\s+|\s+-\s+|\s+)(.+)$`)
var prioritySuffixRe = regexp.MustCompile(`(?i)[,;]?\s*(?:with\s+)?(?:(low|medium|high)\s+priority|priority\s*[:=]?\s*(low|medium|high))$`)
var siteSuffixRe = regexp.MustCompile(`(?i)[,;]?\s*(?:(?:at|on|in|for)\s+)?site\s+([A-Za-z0-9_-]*[0-9][A-Za-z0-9_-]*)$`)
var notATitle = set("list", "lists", "summary", "report", "recap")

func matchCreate(s string) (intent.Command, bool) {
	m := createRe.FindStringSubmatch(s)
	if m == nil {
		return intent.Command{}, false
	}
	rest := strings.TrimSpace(m[1])
	cmd := intent.Command{Intent: intent.CreateTask}
	// Peel "site N" and "high priority" off the end, in whichever order they come.
	for i := 0; i < 3; i++ {
		if p := prioritySuffixRe.FindStringSubmatchIndex(rest); p != nil && cmd.Priority == "" {
			cmd.Priority = strings.ToLower(firstNonEmpty(submatch(rest, p, 1), submatch(rest, p, 2)))
			rest = strings.TrimSpace(rest[:p[0]])
			continue
		}
		if p := siteSuffixRe.FindStringSubmatchIndex(rest); p != nil && cmd.Site == "" && validSiteCode(submatch(rest, p, 1)) {
			cmd.Site = submatch(rest, p, 1)
			rest = strings.TrimSpace(rest[:p[0]])
			continue
		}
		break
	}
	rest = strings.TrimSpace(strings.TrimRight(rest, ",;:- "))
	if len(rest) > 3 && strings.EqualFold(rest[:3], "to ") {
		rest = strings.TrimSpace(rest[3:])
	}
	if rest == "" || notATitle[strings.ToLower(rest)] {
		return intent.Command{}, false
	}
	cmd.Title = rest
	return cmd, true
}

// ---- note -------------------------------------------------------------------

var noteRe = regexp.MustCompile(`(?i)^(?:(?:add|write|leave|attach)\s+(?:a\s+)?note|note|log\s+note)\s+(?:to|on|for|about|under)\s+([^:]+?)\s*:\s*(.+)$`)

func matchNote(s string) (intent.Command, bool) {
	m := noteRe.FindStringSubmatch(s)
	if m == nil {
		return intent.Command{}, false
	}
	ref, ok := cleanRef(m[1])
	if !ok || strings.TrimSpace(m[2]) == "" {
		return intent.Command{}, false
	}
	return intent.Command{Intent: intent.AddNote, TaskRef: ref, Note: strings.TrimSpace(m[2])}, true
}

// ---- update -----------------------------------------------------------------

var statusChangeRe = regexp.MustCompile(`(?i)^(?:mark|set|move|change|update|flag)\s+(.+?)\s+(?:as\s+|to\s+|is\s+)?(done|finished|complete|completed|closed|resolved|in progress|started|underway|open|reopened)$`)
var statusByPhrase = map[string]string{
	"done": "done", "finished": "done", "complete": "done", "completed": "done", "closed": "done", "resolved": "done",
	"in progress": "in_progress", "started": "in_progress", "underway": "in_progress",
	"open": "open", "reopened": "open",
}

func matchStatusChange(s string) (intent.Command, bool) {
	m := statusChangeRe.FindStringSubmatch(s)
	if m == nil {
		return intent.Command{}, false
	}
	ref, ok := cleanRef(m[1])
	if !ok {
		return intent.Command{}, false
	}
	return intent.Command{Intent: intent.UpdateTask, TaskRef: ref, Status: statusByPhrase[strings.ToLower(m[2])]}, true
}

var isStatusRe = regexp.MustCompile(`(?i)^(.+?)\s+(?:is|are)\s+(?:now\s+|finally\s+)?(done|finished|complete|completed|closed|resolved|in progress|started|underway|open|reopened)$`)

func matchIsStatus(s string) (intent.Command, bool) {
	m := isStatusRe.FindStringSubmatch(s)
	if m == nil {
		return intent.Command{}, false
	}
	ref, ok := cleanRef(m[1])
	if !ok {
		return intent.Command{}, false
	}
	return intent.Command{Intent: intent.UpdateTask, TaskRef: ref, Status: statusByPhrase[strings.ToLower(m[2])]}, true
}

var priorityOfRe = regexp.MustCompile(`(?i)^(?:set|change|update|make)\s+(?:the\s+)?priority\s+of\s+(.+?)\s+(?:to|as)\s+(low|medium|high)$`)

func matchPriorityOf(s string) (intent.Command, bool) {
	m := priorityOfRe.FindStringSubmatch(s)
	if m == nil {
		return intent.Command{}, false
	}
	ref, ok := cleanRef(m[1])
	if !ok {
		return intent.Command{}, false
	}
	return intent.Command{Intent: intent.UpdateTask, TaskRef: ref, Priority: strings.ToLower(m[2])}, true
}

var verbChangeRe = regexp.MustCompile(`(?i)^(close|finish|complete|resolve|reopen|start)\s+(.+)$`)
var statusByVerb = map[string]string{
	"close": "done", "finish": "done", "complete": "done", "resolve": "done", "reopen": "open", "start": "in_progress",
}

func matchVerbChange(s string) (intent.Command, bool) {
	m := verbChangeRe.FindStringSubmatch(s)
	if m == nil {
		return intent.Command{}, false
	}
	ref, ok := cleanRef(m[2])
	if !ok {
		return intent.Command{}, false
	}
	return intent.Command{Intent: intent.UpdateTask, TaskRef: ref, Status: statusByVerb[strings.ToLower(m[1])]}, true
}

var priorityChangeRe = regexp.MustCompile(`(?i)^(?:set|change|make|mark|update|raise|lower|bump)\s+(.+?)\s+(?:priority\s+)?(?:as\s+|to\s+)?(low|medium|high)(?:\s+priority)?$`)

func matchPriorityChange(s string) (intent.Command, bool) {
	m := priorityChangeRe.FindStringSubmatch(s)
	if m == nil {
		return intent.Command{}, false
	}
	ref, ok := cleanRef(m[1])
	if !ok {
		return intent.Command{}, false
	}
	return intent.Command{Intent: intent.UpdateTask, TaskRef: ref, Priority: strings.ToLower(m[2])}, true
}

// refDeny lists words that mean the "reference" is really a longer sentence
// (another request, a reason, a place). Better to hand such a message to the
// model than to look up a task called "pump at site 4 because it leaks".
var refDeny = set("new", "create", "add", "log", "note", "notes", "and", "then", "because", "but",
	"also", "at", "site", "for", "to", "that", "which", "when", "if", "priority", "of", "status",
	// questions and pronouns are not names: "what is open" asks, "it is done" cannot be resolved
	"what", "whats", "which", "who", "how", "where", "why", "it", "this", "that", "these", "those", "everything", "all", "there")

// cleanRef strips filler around a task reference and refuses anything that
// does not look like a short name or ID.
func cleanRef(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	for _, p := range []string{"the ", "task ", "ticket ", "job "} {
		for len(ref) > len(p) && strings.EqualFold(ref[:len(p)], p) {
			ref = strings.TrimSpace(ref[len(p):])
		}
	}
	for _, sfx := range []string{" task", " ticket", " job"} {
		if len(ref) > len(sfx) && strings.EqualFold(ref[len(ref)-len(sfx):], sfx) {
			ref = strings.TrimSpace(ref[:len(ref)-len(sfx)])
		}
	}
	words := strings.Fields(ref)
	if len(words) == 0 || len(words) > 5 {
		return "", false
	}
	for _, w := range words {
		if refDeny[strings.ToLower(w)] {
			return "", false
		}
	}
	return ref, true
}

// ---- helpers ----------------------------------------------------------------

// A site code must contain a digit (4, A4, B7). "site visit" and "site for
// today" are ordinary English, not sites; a letters-only site name is left for
// the model, which can read the sentence around it.
var siteCharsRe = regexp.MustCompile(`(?i)^[a-z0-9_-]+$`)

func validSiteCode(s string) bool {
	return len(s) >= 1 && len(s) <= 20 && siteCharsRe.MatchString(s) && strings.ContainsAny(s, "0123456789")
}

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// tokens lower-cases s, turns punctuation into spaces and merges "this week".
func tokens(s string) []string {
	low := strings.ToLower(strings.NewReplacer(",", " ", ";", " ", ":", " ", "'", "", "’", "").Replace(s))
	low = strings.ReplaceAll(low, "this week", "this_week")
	return strings.Fields(low)
}

func isWhen(t string) bool { return t == "today" || t == "yesterday" || t == "this_week" }

func whenWord(s string) string {
	s = strings.ToLower(s)
	if s == "this week" {
		return "this_week"
	}
	return s
}

func submatch(s string, idx []int, n int) string {
	if idx[2*n] < 0 {
		return ""
	}
	return s[idx[2*n]:idx[2*n+1]]
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
