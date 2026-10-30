package main

import (
	"fmt"
	"os"
	"strconv"

	"offgrid/internal/perf"
)

// README markers: `offgrid perf -readme README.md` and `offgrid bench -readme
// README.md` rewrite only what lies between a pair, so the rest of the README
// is never touched.
const (
	perfStart  = "<!-- perf:start -->"
	perfEnd    = "<!-- perf:end -->"
	benchStart = "<!-- bench:start -->"
	benchEnd   = "<!-- bench:end -->"
)

// updateBlock rewrites the block between two marker lines in path, through a
// temporary file, so an error never leaves a half-written README.
func updateBlock(path, start, end, body string) error {
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := perf.ReplaceBlock(string(old), start, end, body)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	perm := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func intList(s string) ([]int, error) {
	var out []int
	for _, p := range splitList(s) {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%q is not a positive whole number", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("an empty list of sizes")
	}
	return out, nil
}
