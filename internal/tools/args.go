package tools

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"offgrid/internal/task"
)

// Text from a model ends up in a database, a terminal and, one day, a web page
// and a log file. Control characters (escape codes, carriage returns, NULs) are
// how text becomes an attack, or just a mess, in all four. The tools refuse
// them rather than trying to clean them.

// oneLine accepts a single line of printable text and collapses repeated
// spaces. A newline in a title is refused, not repaired.
func oneLine(field, s string, p *problems) string {
	if !utf8.ValidString(s) {
		p.addf("%s is not valid text", field)
		return ""
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			p.addf("%s contains a control character (%U), which is not allowed", field, r)
			return ""
		}
	}
	return strings.Join(strings.Fields(s), " ")
}

// freeText accepts printable text that may span lines and contain tabs.
func freeText(field, s string, p *problems) string {
	if !utf8.ValidString(s) {
		p.addf("%s is not valid text", field)
		return ""
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			p.addf("%s contains a control character (%U), which is not allowed", field, r)
			return ""
		}
	}
	return strings.TrimSpace(s)
}

// A site is a short code: letters, digits, dash, underscore. Strict on
// purpose: it is a lookup key, and "4; DROP TABLE" is not a site.
var siteRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,19}$`)

func site(s string, p *problems) string {
	if s == "" {
		return ""
	}
	if !siteRe.MatchString(s) {
		p.addf("site %q is not a valid site code (letters, digits, - and _, up to 20 characters)", s)
		return ""
	}
	return s
}

func priority(s string, p *problems) task.Priority {
	if s == "" {
		return ""
	}
	v, err := task.ParsePriority(s)
	if err != nil {
		p.addf("%v", err)
	}
	return v
}

func status(s string, p *problems) task.Status {
	if s == "" {
		return ""
	}
	v, err := task.ParseStatus(s)
	if err != nil {
		p.addf("%v", err)
	}
	return v
}

func when(s string, p *problems) string {
	if s == "" {
		return ""
	}
	if !task.ValidWhen(s) {
		p.addf("when %q is not one of today, yesterday, this_week", s)
		return ""
	}
	return s
}

// describe is the one-line form of a task used in every message.
func describe(t task.Task) string { return describeAs(task.ShortID(t.ID), t) }

// describeAs is describe with the ID label chosen by the caller (a list picks
// labels so that two tasks never show the same one).
func describeAs(label string, t task.Task) string {
	s := fmt.Sprintf("%s [%s, %s] %s", label, t.Priority, t.Status, t.Title)
	if t.Site != "" {
		s += fmt.Sprintf(" (site %s)", t.Site)
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
