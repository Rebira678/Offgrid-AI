package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"offgrid/internal/sysinfo"
)

// Meta is everything needed to interpret a report later.
type Meta struct {
	Date          string       `json:"date"`
	OllamaVersion string       `json:"ollama_version"`
	Hardware      sysinfo.Info `json:"hardware"`
}

// Setup names what was switched on for a run.
func (s Summary) Setup() string {
	switch {
	case !s.UseSchema && s.MaxAttempts <= 1:
		return "prompt only"
	case s.UseSchema && s.MaxAttempts <= 1:
		return "+ schema"
	case s.UseSchema:
		return fmt.Sprintf("+ schema + retries (up to %d)", s.MaxAttempts)
	default:
		return fmt.Sprintf("prompt + retries (up to %d)", s.MaxAttempts)
	}
}

// maxMisses limits how many failures are listed per run.
const maxMisses = 12

// WriteMarkdown writes the comparison table and the list of misses.
func WriteMarkdown(w io.Writer, meta Meta, sums []Summary) error {
	var b strings.Builder
	hw := meta.Hardware
	fmt.Fprintf(&b, "**Hardware:** %s, %d threads, %.1f GiB RAM  \n", orNA(hw.CPU), hw.Threads, hw.MemGiB)
	fmt.Fprintf(&b, "**Ollama:** %s, **date:** %s, **cases per run:** %d\n\n", orNA(meta.OllamaVersion), meta.Date, firstCases(sums))

	b.WriteString("| Model | Setup | Clean JSON, 1st try | Valid, 1st try | Valid after retries | Intent correct | Arguments correct | Avg tries | Median s/case |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range sums {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %.2f | %.1f |\n",
			s.Model, s.Setup(), s.CleanRate(), s.ValidFirstRate(), s.SucceededRate(),
			s.IntentRate(), s.ArgsRate(), s.AvgAttempts(), s.MedianSeconds)
	}
	b.WriteString("\nFormat: correct/total = percent [95% confidence interval for the true rate]. ")
	b.WriteString("With this few cases the intervals are wide: two results whose intervals overlap cannot be ranked.\n")
	b.WriteString("\"Clean\" means the first reply was exactly a JSON object of the right shape, with nothing around it. ")
	b.WriteString("\"Valid\" also allows cutting off surrounding text, and requires the content checks to pass. ")
	b.WriteString("\"Arguments correct\" counts only the cases that list expected arguments, and requires the right intent.\n")

	for _, s := range sums {
		misses := missesOf(s)
		if len(misses) == 0 && s.Repaired == 0 && s.Errors == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s, %s\n\n", s.Model, s.Setup())
		if s.Repaired > 0 {
			fmt.Fprintf(&b, "- %d case(s) needed text cut off around the JSON before it could be read.\n", s.Repaired)
		}
		if s.Errors > 0 {
			fmt.Fprintf(&b, "- %d case(s) failed for reasons other than a bad reply (for example a timeout).\n", s.Errors)
		}
		for i, m := range misses {
			if i == maxMisses {
				fmt.Fprintf(&b, "- ...and %d more (see the JSON report)\n", len(misses)-maxMisses)
				break
			}
			fmt.Fprintf(&b, "- %s %q: %s\n", m.ID, m.Text, describe(m))
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes the full results, including every case, for later analysis.
func WriteJSON(w io.Writer, meta Meta, sums []Summary) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Meta Meta      `json:"meta"`
		Runs []Summary `json:"runs"`
	}{meta, sums})
}

func missesOf(s Summary) []CaseResult {
	var out []CaseResult
	for _, r := range s.Results {
		if !r.IntentCorrect || (r.ArgsChecked && !r.ArgsCorrect) {
			out = append(out, r)
		}
	}
	return out
}

func describe(r CaseResult) string {
	switch {
	case !r.Succeeded:
		return fmt.Sprintf("no valid reply after %d attempt(s): %s", r.Attempts, r.Error)
	case !r.IntentCorrect:
		return fmt.Sprintf("expected %s, got %s", r.Expected, r.Got)
	default:
		return fmt.Sprintf("intent %s was right but an argument differed from the expected value", r.Expected)
	}
}

func firstCases(sums []Summary) int {
	if len(sums) == 0 {
		return 0
	}
	return sums[0].Cases
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}
