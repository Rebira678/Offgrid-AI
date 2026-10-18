package dash

import (
	"context"
	"io"
	"strings"
	"time"
)

// Terminal control sequences.
const (
	altScreenOn  = "\x1b[?1049h"
	altScreenOff = "\x1b[?1049l"
	hideCursor   = "\x1b[?25l"
	showCursor   = "\x1b[?25h"
	home         = "\x1b[H"
	clearBelow   = "\x1b[J"
	clearToEOL   = "\x1b[K"
)

// Live repaints a frame every interval until ctx ends. next builds the View
// for each frame. If tty is true it takes over the screen (alternate screen,
// hidden cursor) and always puts the terminal back, even when ctx ends
// mid-frame; otherwise it prints each frame one after another, separated by a
// blank line, so output piped to a file or a pager stays readable.
func Live(ctx context.Context, w io.Writer, tty bool, interval time.Duration, next func() (View, Options)) {
	if interval <= 0 {
		interval = time.Second
	}
	if tty {
		io.WriteString(w, altScreenOn+hideCursor)
		defer io.WriteString(w, showCursor+altScreenOff)
	}
	paint := func() {
		v, o := next()
		frame := Render(v, o)
		if !tty {
			io.WriteString(w, frame+"\n\n")
			return
		}
		// Overwrite in place: move home, end every line by clearing the rest
		// of it, then clear what is left of the previous (longer) frame.
		io.WriteString(w, home+strings.ReplaceAll(frame, "\n", clearToEOL+"\n")+clearToEOL+"\n"+
			"\x1b[2m(Ctrl-C to quit)\x1b[0m"+clearToEOL+clearBelow)
	}
	paint()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			paint()
		}
	}
}
