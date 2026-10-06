package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"offgrid/internal/sysinfo"
)

// Meta is everything needed to interpret a benchmark later.
type Meta struct {
	Date          string       `json:"date"`
	OllamaVersion string       `json:"ollama_version"`
	Hardware      sysinfo.Info `json:"hardware"`
	Runs          int          `json:"runs_per_cell"`
}

// WriteMarkdown writes the results as a Markdown table that can be pasted
// straight into the README.
func WriteMarkdown(w io.Writer, meta Meta, rows []Row) error {
	var b strings.Builder
	hw := meta.Hardware
	fmt.Fprintf(&b, "**Hardware:** %s, %d threads, %.1f GiB RAM, %s  \n", orNA(hw.CPU), hw.Threads, hw.MemGiB, orNA(hw.OS))
	fmt.Fprintf(&b, "**Ollama:** %s, **runs per cell:** %d (median), **date:** %s\n\n", orNA(meta.OllamaVersion), meta.Runs, meta.Date)

	b.WriteString("| Model | Prompt | Load (s) | Time to first token (s) | Generation (tok/s) | Prompt processing (tok/s) | Prompt / generated tokens |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		if r.Runs == 0 {
			fmt.Fprintf(&b, "| %s | %s | failed | failed | failed | failed | failed |\n", r.Model, r.Prompt)
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %d / %d |\n",
			r.Model, r.Prompt, num(r.LoadSeconds), num(r.TTFTSeconds),
			num(r.GenTokPerSec), num(r.PromptTokPerSec), r.PromptTokens, r.GenTokens)
	}

	var failures []string
	for _, r := range rows {
		if r.Error != "" {
			failures = append(failures, fmt.Sprintf("- %s (%s): %s", r.Model, r.Prompt, r.Error))
		}
	}
	if len(failures) > 0 {
		b.WriteString("\n**Errors:**\n")
		b.WriteString(strings.Join(failures, "\n"))
		b.WriteString("\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes the same results in a machine-readable form, which later
// days will use to draw charts and detect speed regressions.
func WriteJSON(w io.Writer, meta Meta, rows []Row) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Meta Meta  `json:"meta"`
		Rows []Row `json:"rows"`
	}{meta, rows})
}

func num(v float64) string {
	if v <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", v)
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}
