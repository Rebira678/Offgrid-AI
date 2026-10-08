package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"offgrid/internal/intent"
	"offgrid/internal/llm"
	"offgrid/internal/ollama"
	"offgrid/internal/structured"
)

// cmdParse turns one worker message into a validated command and prints it as
// JSON. It is the live version of what the evaluation measures in bulk: try it
// on a message, see exactly what the model said and why anything was rejected.
func cmdParse(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("parse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", defaultHost(), "Ollama server URL (default: $OLLAMA_HOST or http://127.0.0.1:11434)")
	model := fs.String("model", envOr("OFFGRID_MODEL", defaultModel), "model to use (default: $OFFGRID_MODEL or "+defaultModel+")")
	schema := fs.Bool("schema", true, "constrain the model's reply to the command schema (-schema=false to test the prompt alone)")
	attempts := fs.Int("attempts", structured.DefaultMaxAttempts, "most times the model may try, the first included")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up if there is no result by then")
	verbose := fs.Bool("v", false, "show every attempt: what the model said and why it was accepted or rejected")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *attempts < 1 {
		fmt.Fprintln(stderr, "error: -attempts must be at least 1")
		return 2
	}

	text, err := readPrompt(fs.Args(), stdin)
	if err != nil {
		fmt.Fprintf(stderr, "error: reading standard input: %v\n", err)
		return 1
	}
	if text == "" {
		fmt.Fprintln(stderr, `error: no message given. Try: offgrid parse "log a leaking pump at site 4, high priority"`)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	classifier := intent.Classifier{
		Client:      llm.NewOllama(ollama.NewClient(normalizeHost(*host))),
		Model:       *model,
		UseSchema:   *schema,
		MaxAttempts: *attempts,
	}
	res, err := classifier.Classify(ctx, text)

	if *verbose || err != nil {
		for i, at := range res.Attempts {
			verdict := "accepted"
			if at.Err != nil {
				verdict = "rejected: " + at.Err.Error()
			}
			note := ""
			if at.Repaired {
				note = " (text around the JSON was cut off)"
			}
			fmt.Fprintf(stderr, "attempt %d%s: %s\n  -> %s\n", i+1, note, at.Raw, verdict)
		}
	}

	if err != nil {
		if errors.Is(err, structured.ErrNoValidReply) {
			fmt.Fprintf(stderr, "\nerror: the model produced no valid command in %d attempt(s).\n", len(res.Attempts))
			fmt.Fprintln(stderr, "Try -v to see each attempt, a larger model, or -schema=true if you turned it off.")
			return 1
		}
		return reportAskError(stderr, err, *model, *timeout)
	}

	out, _ := json.MarshalIndent(res.Value, "", "  ") // a struct of strings cannot fail to encode
	fmt.Fprintln(stdout, string(out))
	fmt.Fprintf(stderr, "(%d attempt(s), %s)\n", len(res.Attempts), *model)
	return 0
}
