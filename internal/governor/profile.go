// Package governor keeps a node inside the machine it runs on (Day 51).
//
// Two profiles, because two kinds of machine are used:
//
//	light  a Raspberry Pi or any small box: sync, web, rules and the queue. It does not run a model.
//	       A 3B model on a Pi is too slow to be useful, so it asks a full node for AI (a remote provider)
//	       or does without: the router already turns "no model" into "queued for later".
//	full   a laptop or mini-PC with a local model.
//
// The governor does four things: sets the Go runtime's soft memory limit, limits how many model calls
// run at once (and how many may wait), refuses background work while the machine is busy or on
// battery, and, only when asked, serves pprof on a loopback port. Everything that it refuses is
// reported as llm.ErrUnavailable, which the router and the agent already treat as "degrade, do not
// crash".
package governor

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"offgrid/internal/sysinfo"
)

// Profile is a node's role.
type Profile string

// The profiles.
const (
	Light Profile = "light"
	Full  Profile = "full"
)

// Config is what a profile sets. Every field can be overridden by a flag; zero in an override means
// "use the profile's value".
type Config struct {
	Profile     Profile
	MemLimit    int64         // soft limit for the Go runtime, bytes (debug.SetMemoryLimit)
	LocalSlots  int           // model calls at once against a model on this machine; 0 means none
	RemoteSlots int           // model calls at once against a model on another node
	MaxQueue    int           // how many more calls may wait for a slot before the next is refused
	MaxWait     time.Duration // how long a call may wait for a slot
	// MaxBusy is the CPU share (0..1) above which background work waits. 0 means the idle package default.
	MaxBusy float64
	// AllowOnBattery lets background model work run on battery. By default it waits for the charger.
	AllowOnBattery bool
}

const mib = 1 << 20

// Defaults returns the configuration of a profile.
//
// The memory limits are for THIS process. A local model server (Ollama, llama.cpp) is a separate
// process with its own memory and is not covered; budget for it separately. The limit is soft: near it
// the runtime collects more often and the node gets slower, it does not die. The hard check is the
// soak's budget (offgrid sync stats -max-rss).
func Defaults(p Profile) Config {
	switch p {
	case Light:
		return Config{Profile: Light, MemLimit: 96 * mib, LocalSlots: 0, RemoteSlots: 1, MaxQueue: 2, MaxWait: 20 * time.Second}
	default:
		return Config{Profile: Full, MemLimit: 768 * mib, LocalSlots: 1, RemoteSlots: 2, MaxQueue: 4, MaxWait: 60 * time.Second}
	}
}

// ParseProfile reads "light", "full" or "auto" ("" is "auto"). Auto picks light on a small or ARM
// machine and full otherwise, from what the machine says about itself; it is a default, and the
// profile is always printed at start so nobody has to guess which one they got.
func ParseProfile(s string, info sysinfo.Info) (Profile, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "light":
		return Light, nil
	case "full":
		return Full, nil
	case "", "auto":
		return Auto(info, runtime.GOARCH), nil
	}
	return "", fmt.Errorf("governor: unknown profile %q (use light, full or auto)", s)
}

// Auto chooses a profile: light when the machine is ARM (a Pi, a phone-class board), has under 8 GiB
// of memory, or has under 4 threads; full otherwise. Unknown memory counts as small: wrongly choosing
// light costs a slower assistant; wrongly choosing full costs a crashed node.
func Auto(info sysinfo.Info, goarch string) Profile {
	if strings.HasPrefix(goarch, "arm") || info.MemGiB < 8 || info.Threads < 4 {
		return Light
	}
	return Full
}
