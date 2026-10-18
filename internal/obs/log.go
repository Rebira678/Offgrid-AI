package obs

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"offgrid/internal/connectivity"
	"offgrid/internal/drain"
	"offgrid/internal/router"
)

// NewLogger builds a slog logger. format is "text" or "json"; level is
// debug, info, warn or error. Timestamps use the logger's own clock.
func NewLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "", "info":
		lv = slog.LevelInfo
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("-log-level must be debug, info, warn or error (got %q)", level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch strings.ToLower(format) {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("-log-format must be text or json (got %q)", format)
}

// Observer feeds one stream of happenings to both the log and the counters.
// Either part may be nil. A nil *Observer does nothing, so call sites need
// no checks.
type Observer struct {
	Log *slog.Logger
	M   *Metrics
}

func (o *Observer) log() *slog.Logger {
	if o == nil || o.Log == nil {
		return nil
	}
	return o.Log
}

// Drain handles a worker event: log it at a level that matches how bad it is.
func (o *Observer) Drain(e drain.Event) {
	if o == nil {
		return
	}
	if o.M != nil {
		o.M.Drain(e)
	}
	l := o.log()
	if l == nil {
		return
	}
	attrs := []any{"job", e.Job.ID, "kind", e.Job.Kind, "attempt", e.Job.Attempts, "max_attempts", e.Job.MaxAttempts}
	switch e.Kind {
	case drain.EvClaimed:
		l.Info("job started", attrs...)
	case drain.EvDone:
		l.Info("job done", append(attrs, "result", oneLine(e.Note, 200))...)
	case drain.EvRetry:
		l.Warn("job failed, will retry", append(attrs, "retry_in", e.Delay, "err", errText(e.Err))...)
	case drain.EvDead:
		l.Error("job dead", append(attrs, "err", errText(e.Err))...)
	case drain.EvReleased:
		l.Warn("job given back", append(attrs, "why", e.Note)...)
	case drain.EvLostClaim:
		l.Warn("job lost its claim", append(attrs, "note", e.Note)...)
	case drain.EvGate:
		l.Info("network gate", "note", e.Note)
	case drain.EvError:
		l.Error("queue problem", append(attrs, "err", errText(e.Err))...)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Probe handles one network check; it matches connectivity.Config.OnProbe.
func (o *Observer) Probe(r connectivity.Result, v connectivity.Verdict, s connectivity.State) {
	if o == nil {
		return
	}
	if o.M != nil {
		o.M.Probe(r, v, s)
	}
	if l := o.log(); l != nil {
		l.Debug("probe", "ok", r.OK, "latency", r.Latency, "target", r.Target, "verdict", v.String(), "state", s.String(), "err", errText(r.Err))
	}
}

// Transition handles a connectivity change.
func (o *Observer) Transition(e connectivity.Event) {
	if o == nil {
		return
	}
	if o.M != nil {
		o.M.Transition(e)
	}
	if l := o.log(); l != nil {
		l.Info("network state changed", "from", e.From.String(), "to", e.To.String(), "reason", e.Reason, "probe_latency", e.Last.Latency)
	}
}

// Forced records a connectivity state that is not probed.
func (o *Observer) Forced(s connectivity.State, why string) {
	if o == nil {
		return
	}
	if o.M != nil {
		o.M.SetNet(s, why)
	}
	if l := o.log(); l != nil {
		l.Info("network state fixed", "state", s.String(), "why", why)
	}
}

// Route handles one routed message.
func (o *Observer) Route(r router.Result) {
	if o == nil {
		return
	}
	if o.M != nil {
		o.M.Route(r)
	}
	l := o.log()
	if l == nil {
		return
	}
	attrs := []any{"tier", TierOf(r), "route", string(r.Route), "reason", r.Reason, "net", r.Net.String(), "took", r.Total, "text", oneLine(r.Text, 80)}
	if r.Job != nil {
		attrs = append(attrs, "job", r.Job.ID, "new", r.Created)
	}
	if r.Err != nil {
		l.Error("routed", append(attrs, "err", r.Err.Error())...)
		return
	}
	l.Info("routed", attrs...)
}
