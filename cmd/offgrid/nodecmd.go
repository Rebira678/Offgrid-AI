package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"offgrid/internal/governor"
	"offgrid/internal/idle"
	"offgrid/internal/llm"
	"offgrid/internal/sysinfo"
)

// activeGov is the governor of this process, set by the long-running commands (Day 51). chatClient puts
// every model call under it; drain asks it before taking a slow job. nil means no governor (a one-shot
// command such as ask or eval), and then nothing changes from before.
var activeGov atomic.Pointer[governor.Governor]

// nodeFlags are the resource flags that every long-running command takes.
type nodeFlags struct {
	profile      *string
	memLimit     *int
	slots        *int
	allowBattery *bool
	pprof        *string
}

const profileHelp = `node profile: "full" (a local model may run here), "light" (a small box: no local model; ask a full node with -host llamacpp@URL or ollama@URL), or "auto" (light on ARM or under 8 GiB, else full). Default: $OFFGRID_PROFILE or full`

func addNodeFlags(fs *flag.FlagSet) nodeFlags {
	def := os.Getenv("OFFGRID_PROFILE")
	if def == "" {
		def = "full"
	}
	return nodeFlags{
		profile:      fs.String("profile", def, profileHelp),
		memLimit:     fs.Int("mem-limit", 0, "soft memory limit for this process in MiB (default: the profile's; $GOMEMLIMIT wins over both)"),
		slots:        fs.Int("model-slots", 0, "model calls allowed at once against a model on this machine (default: the profile's)"),
		allowBattery: fs.Bool("allow-battery", false, "let slow background model work run while on battery (default: it waits for the charger)"),
		pprof:        fs.String("pprof", "", "serve pprof on this loopback address (for example 127.0.0.1:6060); off by default"),
	}
}

// start applies the profile and returns a function that undoes what it set up. It prints one line that
// says which profile is in force, so it is never a guess.
func (f nodeFlags) start(stderr io.Writer, role string) (stop func(), code int) {
	p, err := governor.ParseProfile(*f.profile, sysinfo.Detect())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, 2
	}
	if *f.memLimit < 0 || *f.slots < 0 {
		fmt.Fprintln(stderr, "error: -mem-limit and -model-slots must not be negative")
		return nil, 2
	}
	cfg := governor.Defaults(p)
	if *f.memLimit > 0 {
		cfg.MemLimit = int64(*f.memLimit) << 20
	}
	if *f.slots > 0 {
		if p == governor.Light {
			fmt.Fprintln(stderr, "note: -model-slots is ignored on the light profile (it has no local model)")
		} else {
			cfg.LocalSlots = *f.slots
		}
	}
	cfg.AllowOnBattery = *f.allowBattery
	g := governor.New(cfg, governor.Sysfs{}, (&idle.Sampler{MaxBusy: cfg.MaxBusy}).Quiet)
	line := g.Start()
	fmt.Fprintf(stderr, "%s: %s\n", role, line)
	var pp *governor.Pprof
	if *f.pprof != "" {
		if pp, err = governor.ServePprof(*f.pprof); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return nil, 2
		}
		fmt.Fprintf(stderr, "%s: pprof on http://%s/debug/pprof/ (this machine only)\n", role, pp.Addr)
	}
	activeGov.Store(g)
	return func() {
		activeGov.Store(nil)
		if pp != nil {
			_ = pp.Close()
		}
	}, 0
}

// governed puts c under the process's governor, if there is one.
func governed(c llm.Client, remote bool) llm.Client {
	if g := activeGov.Load(); g != nil {
		return g.Wrap(c, remote)
	}
	return c
}

// idleGate is the drain loop's gate: the CPU must be quiet and, unless allowed, the charger plugged in.
func idleGate(maxBusy float64) func() bool { return gateOf(&idle.Sampler{MaxBusy: maxBusy}) }

func gateOf(s *idle.Sampler) func() bool {
	return func() bool {
		if g := activeGov.Load(); g != nil && g.OnBatteryBlocked() {
			return false
		}
		return s.Quiet()
	}
}
