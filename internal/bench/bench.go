// Package bench measures how fast local models run on this machine.
//
// Method, and why:
//   - One warm-up request per model. The first request pays the cost of
//     loading the model from disk into RAM. We record that as "load time" and
//     keep it out of the speed numbers.
//   - Several timed runs per prompt, reported as the MEDIAN. A single run is
//     noisy (another process wakes up, the CPU throttles); the mean is dragged
//     around by one bad run; the median shrugs it off.
//   - A unique "Request ID" line at the start of every prompt. Ollama reuses
//     the cached prefix of an identical prompt and skips re-reading it, which
//     would make prompt processing look impossibly fast. A different first
//     line defeats the cache so every run does the real work.
//   - Temperature 0 and a fixed seed, so runs are as repeatable as possible.
package bench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"offgrid/internal/ollama"
)

// keepAlive keeps the model in RAM between runs so only the warm-up pays the
// load cost.
const keepAlive = "10m"

// Generator is the one thing the benchmark needs from an LLM backend. It is
// declared here, next to its only consumer, instead of next to the Ollama
// client. That is the idiomatic Go way: the consumer states what it needs, so
// tests can pass a tiny fake and the benchmark never depends on HTTP.
// *ollama.Client satisfies it automatically.
type Generator interface {
	Generate(ctx context.Context, req ollama.GenerateRequest, onToken func(string)) (ollama.Result, error)
}

// Prompt is one benchmark workload.
type Prompt struct {
	Name      string
	Text      string
	MaxTokens int
}

// Config controls a benchmark run.
type Config struct {
	Models     []string
	Prompts    []Prompt
	Runs       int           // timed runs per model and prompt (after warm-up)
	RunTimeout time.Duration // limit for a single request; 0 = none
	Salt       string        // makes prompts unique across invocations
	Progress   func(format string, args ...any)
}

// Row is one line of the results table: one model on one prompt.
type Row struct {
	Model           string  `json:"model"`
	Prompt          string  `json:"prompt"`
	Runs            int     `json:"runs"`
	LoadSeconds     float64 `json:"load_seconds"`
	TTFTSeconds     float64 `json:"ttft_seconds"`
	GenTokPerSec    float64 `json:"gen_tokens_per_sec"`
	PromptTokPerSec float64 `json:"prompt_tokens_per_sec"`
	PromptTokens    int     `json:"prompt_tokens"`
	GenTokens       int     `json:"gen_tokens"`
	Error           string  `json:"error,omitempty"`
}

// Run benchmarks every model in cfg and returns one Row per model and prompt.
// A model that fails does not stop the others; its failure is recorded in the
// Row. The only error Run itself returns is a cancelled context, together with
// whatever rows were finished before that.
func Run(ctx context.Context, g Generator, cfg Config) ([]Row, error) {
	if cfg.Runs < 1 {
		return nil, errors.New("bench: runs must be at least 1")
	}
	if len(cfg.Models) == 0 {
		return nil, errors.New("bench: no models given")
	}
	if len(cfg.Prompts) == 0 {
		return nil, errors.New("bench: no prompts given")
	}
	progress := cfg.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}

	var rows []Row
	for _, model := range cfg.Models {
		if err := ctx.Err(); err != nil {
			return rows, err
		}

		progress("[%s] warming up (loading the model into RAM)...", model)
		warm, err := call(ctx, g, cfg, ollama.GenerateRequest{
			Model:     model,
			Prompt:    "Say hi.",
			KeepAlive: keepAlive,
			Options:   ollama.Options{NumPredict: 8, Temperature: 0, Seed: 1},
		})
		if err != nil {
			if ctx.Err() != nil {
				return rows, ctx.Err()
			}
			progress("[%s] failed during warm-up: %v", model, err)
			rows = append(rows, Row{Model: model, Prompt: "(warm-up)", Error: err.Error()})
			continue
		}
		progress("[%s] loaded in %.1fs", model, warm.Load.Seconds())

		for _, p := range cfg.Prompts {
			row := Row{Model: model, Prompt: p.Name, LoadSeconds: warm.Load.Seconds()}
			var ttft, gen, pre, pTok, gTok []float64

			for i := 1; i <= cfg.Runs; i++ {
				progress("[%s] %s prompt, run %d/%d", model, p.Name, i, cfg.Runs)
				res, err := call(ctx, g, cfg, ollama.GenerateRequest{
					Model:     model,
					Prompt:    fmt.Sprintf("Request ID: %s/%s/%d.\n\n%s", cfg.Salt, p.Name, i, p.Text),
					KeepAlive: keepAlive,
					Options:   ollama.Options{NumPredict: p.MaxTokens, Temperature: 0, Seed: 42},
				})
				if err != nil {
					if ctx.Err() != nil {
						return rows, ctx.Err()
					}
					progress("[%s] %s prompt failed: %v", model, p.Name, err)
					row.Error = err.Error()
					break
				}
				ttft = append(ttft, res.TimeToFirstToken.Seconds())
				gen = append(gen, res.GenTokensPerSec())
				pre = append(pre, res.PromptTokensPerSec())
				pTok = append(pTok, float64(res.PromptTokens))
				gTok = append(gTok, float64(res.GenTokens))
				row.Runs++
			}

			if row.Runs > 0 {
				row.TTFTSeconds = median(ttft)
				row.GenTokPerSec = median(gen)
				row.PromptTokPerSec = median(pre)
				row.PromptTokens = int(math.Round(median(pTok)))
				row.GenTokens = int(math.Round(median(gTok)))
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// call runs one request under the per-request timeout.
func call(ctx context.Context, g Generator, cfg Config, req ollama.GenerateRequest) (ollama.Result, error) {
	if cfg.RunTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.RunTimeout)
		defer cancel()
	}
	return g.Generate(ctx, req, nil)
}

// median returns the middle value (or the mean of the two middle values).
// It does not modify its argument. An empty slice gives 0.
func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
