package governor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"

	"offgrid/internal/llm"
)

// Kind says why a call was refused.
type Kind string

// The kinds of refusal.
const (
	KindModelOff Kind = "model-off" // the light profile has no local model
	KindNoSlots  Kind = "no-slots"  // a limiter with zero slots
	KindBusy     Kind = "busy"      // slots full and the queue full or the wait over
	KindBackoff  Kind = "backoff"   // background work, machine busy or on battery
)

// Refusal is a model call that was not made. It is an llm.ErrUnavailable, because that is exactly
// what it means to the caller: no answer from a model right now, degrade (rules, queue), do not fail.
type Refusal struct {
	Kind   Kind
	Reason string
}

func (r *Refusal) Error() string { return "governor: " + r.Reason }

// Unwrap makes errors.Is(err, llm.ErrUnavailable) true.
func (r *Refusal) Unwrap() error { return llm.ErrUnavailable }

// Governor applies a Config to one process.
type Governor struct {
	cfg    Config
	power  Power
	quiet  func() bool // true when the CPU is quiet enough for background work
	local  *limiter
	remote *limiter
}

// New builds a governor. quiet may be nil (always quiet); power may be nil (never on battery).
func New(cfg Config, power Power, quiet func() bool) *Governor {
	if power == nil {
		power = FixedPower(false)
	}
	if quiet == nil {
		quiet = func() bool { return true }
	}
	return &Governor{cfg: cfg, power: power, quiet: quiet,
		local:  newLimiter("local", cfg.LocalSlots, cfg.MaxQueue, cfg.MaxWait),
		remote: newLimiter("remote", cfg.RemoteSlots, cfg.MaxQueue, cfg.MaxWait)}
}

// Config returns the configuration in force.
func (g *Governor) Config() Config { return g.cfg }

// Apply sets the runtime's soft memory limit from the profile. A GOMEMLIMIT in the environment wins:
// someone who set it meant it. It returns the limit now in force and where it came from.
func (g *Governor) Apply() (limit int64, source string) {
	if v := os.Getenv("GOMEMLIMIT"); v != "" {
		return debug.SetMemoryLimit(-1), "GOMEMLIMIT"
	}
	if g.cfg.MemLimit > 0 {
		debug.SetMemoryLimit(g.cfg.MemLimit)
		return g.cfg.MemLimit, "profile " + string(g.cfg.Profile)
	}
	return debug.SetMemoryLimit(-1), "none"
}

// BackgroundOK says whether slow, optional work may start now, and if not why not. This is what the
// drain loop asks before it takes a job: a person standing at the screen is never made to wait for it.
func (g *Governor) BackgroundOK() (bool, string) {
	if !g.cfg.AllowOnBattery && g.power.OnBattery() {
		return false, "on battery: background model work waits for the charger"
	}
	if !g.quiet() {
		return false, "the machine is busy: background model work waits until it is quiet"
	}
	return true, ""
}

// OnBatteryBlocked reports whether background work is held back by the battery rule right now.
func (g *Governor) OnBatteryBlocked() bool { return !g.cfg.AllowOnBattery && g.power.OnBattery() }

type bgKey struct{}

// Background marks ctx as belonging to background work, so model calls made under it obey
// BackgroundOK as well as the slot limits.
func Background(ctx context.Context) context.Context { return context.WithValue(ctx, bgKey{}, true) }

func isBackground(ctx context.Context) bool { v, _ := ctx.Value(bgKey{}).(bool); return v }

type client struct {
	next   llm.Client
	g      *Governor
	remote bool
}

// Wrap returns c under the governor. remote says whether the model is on another node: a remote model
// costs this machine nothing, so it is not refused on a light profile, on battery or when busy, and it
// has its own slots.
func (g *Governor) Wrap(c llm.Client, remote bool) llm.Client { return &client{c, g, remote} }

func (c *client) Chat(ctx context.Context, req llm.Request, onToken func(string)) (llm.Response, error) {
	g := c.g
	if !c.remote && g.cfg.Profile == Light {
		return llm.Response{}, &Refusal{Kind: KindModelOff,
			Reason: "this is a light node and has no local model; point -host at a full node (llamacpp@URL or ollama@URL) to use AI"}
	}
	if isBackground(ctx) && !c.remote {
		if ok, why := g.BackgroundOK(); !ok {
			return llm.Response{}, &Refusal{Kind: KindBackoff, Reason: why}
		}
	}
	l := g.local
	if c.remote {
		l = g.remote
	}
	release, err := l.acquire(ctx)
	if err != nil {
		return llm.Response{}, err
	}
	defer release()
	return c.next.Chat(ctx, req, onToken)
}

// Start applies the memory limit and returns the line a command prints at start, so the profile in
// force is never a guess.
func (g *Governor) Start() string {
	limit, src := g.Apply()
	lim := "no memory limit"
	if limit > 0 && limit < 1<<62 {
		lim = fmt.Sprintf("memory limit %d MiB (%s)", limit/mib, src)
	}
	model := fmt.Sprintf("%d local and %d remote model call(s) at once", g.cfg.LocalSlots, g.cfg.RemoteSlots)
	if g.cfg.Profile == Light {
		model = "no local model, " + fmt.Sprintf("%d remote call(s) at once", g.cfg.RemoteSlots)
	}
	return fmt.Sprintf("profile %s: %s, %s", g.cfg.Profile, lim, model)
}

// IsRefusal reports whether err is a governor refusal, and which kind.
func IsRefusal(err error) (Kind, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Kind, true
	}
	return "", false
}
