package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"offgrid/internal/llm"
	"offgrid/internal/ollama"
)

const defaultModel = "llama3.2:3b"

// askOptions is everything runAsk needs. It holds no Ollama types: runAsk works
// with any llm.Client.
type askOptions struct {
	Model       string
	System      string
	Prompt      string
	Temperature float64
	MaxTokens   int
	Stats       bool
}

// cmdAsk is the thin layer around runAsk that deals with the outside world:
// flags, the prompt source, signals, the timeout, and which backend to use.
// Everything interesting is in runAsk, which tests can drive with a fake model.
func cmdAsk(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", defaultHost(), "Ollama server URL (default: $OLLAMA_HOST or http://127.0.0.1:11434)")
	model := fs.String("model", envOr("OFFGRID_MODEL", defaultModel), "model to use (default: $OFFGRID_MODEL or "+defaultModel+")")
	system := fs.String("system", "", "system prompt that frames the conversation")
	temp := fs.Float64("temp", 0.2, "temperature: 0 is most predictable, higher is more varied")
	maxTokens := fs.Int("max-tokens", 512, "maximum length of the answer in tokens (0 = no limit)")
	timeout := fs.Duration("timeout", 3*time.Minute, "give up if there is no complete answer by then")
	stats := fs.Bool("stats", false, "print speed statistics to stderr when done")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	prompt, err := readPrompt(fs.Args(), stdin)
	if err != nil {
		fmt.Fprintf(stderr, "error: reading standard input: %v\n", err)
		return 1
	}
	if prompt == "" {
		fmt.Fprintln(stderr, `error: no prompt given. Try: offgrid ask "what is a CRDT?"`)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	// This is the only line that knows the backend is Ollama. Replacing it is
	// all it takes to point the whole program at a different model server.
	var client llm.Client = llm.NewOllama(ollama.NewClient(normalizeHost(*host)))

	err = runAsk(ctx, client, askOptions{
		Model:       *model,
		System:      *system,
		Prompt:      prompt,
		Temperature: *temp,
		MaxTokens:   *maxTokens,
		Stats:       *stats,
	}, stdout, stderr)
	if err != nil {
		return reportAskError(stderr, err, *model, *timeout)
	}
	return 0
}

// runAsk sends one question to c and prints the answer as it streams in.
// The answer goes to stdout; statistics go to stderr, so
// `offgrid ask ... > answer.txt` captures only the answer.
func runAsk(ctx context.Context, c llm.Client, o askOptions, stdout, stderr io.Writer) error {
	var msgs []llm.Message
	if strings.TrimSpace(o.System) != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: o.System})
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: o.Prompt})

	var wrote bool
	var lastByte byte
	endLine := func() {
		if wrote && lastByte != '\n' {
			fmt.Fprintln(stdout)
		}
	}

	resp, err := c.Chat(ctx, llm.Request{
		Model:       o.Model,
		Messages:    msgs,
		MaxTokens:   o.MaxTokens,
		Temperature: o.Temperature,
	}, func(piece string) {
		if piece == "" {
			return
		}
		fmt.Fprint(stdout, piece)
		wrote = true
		lastByte = piece[len(piece)-1]
	})
	endLine() // also after a failure, so a half-printed answer never glues onto the next prompt
	if err != nil {
		return err
	}

	if o.Stats {
		u := resp.Usage
		fmt.Fprintf(stderr, "\n%s | %d tokens | %s tok/s | first token %.2fs | total %.2fs\n",
			resp.Model, u.CompletionTokens, rateText(u.TokensPerSecond()),
			u.TimeToFirstToken.Seconds(), u.Total.Seconds())
	}
	return nil
}

// reportAskError turns an error into advice and an exit code.
// 2 = the command was used wrongly, 130 = interrupted (the shell convention
// for Ctrl-C), 1 = anything else.
func reportAskError(w io.Writer, err error, model string, timeout time.Duration) int {
	var notFound *llm.ModelNotFoundError
	switch {
	case errors.Is(err, llm.ErrInvalidRequest):
		fmt.Fprintf(w, "error: %v\n", err)
		return 2
	case errors.Is(err, llm.ErrUnavailable):
		fmt.Fprintf(w, "error: %v\n\nIs Ollama running? Start it with: ollama serve\n(or check: systemctl status ollama)\n", err)
		return 1
	case errors.As(err, &notFound):
		fmt.Fprintf(w, "error: model %q is not installed.\nInstall it with: ollama pull %s\nSee what you have: offgrid models\n", notFound.Model, notFound.Model)
		return 1
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(w, "error: no complete answer within %s. A model that was not loaded yet can take a while;\ntry a smaller model, or a longer -timeout.\n", timeout)
		return 1
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(w, "cancelled")
		return 130
	default:
		fmt.Fprintf(w, "error: %v\n", err)
		return 1
	}
}

func rateText(v float64) string {
	if v <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", v)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// readPrompt returns the words given on the command line, or, if there are
// none and input is being piped in, whatever is on standard input. It returns
// "" when there is nothing to use.
func readPrompt(args []string, stdin io.Reader) (string, error) {
	prompt := strings.TrimSpace(strings.Join(args, " "))
	if prompt != "" || isTerminal(stdin) {
		return prompt, nil
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// isTerminal reports whether r is an interactive keyboard. When it is, we must
// not try to read a prompt from it: ReadAll would sit there waiting forever.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
