//go:build ignore

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"offgrid/internal/jobqueue"
	"offgrid/internal/obs"
)

func main() {
	metricsDir := "./metrics"
	if len(os.Args) > 1 {
		metricsDir = os.Args[1]
	}
	if err := os.MkdirAll(metricsDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	now := time.Now().UTC()
	probeLat := []int64{12, 14, 15, 13, 16, 14, 18, 14, 12, 15, 13, 15, 14}

	drainSnap := obs.Snapshot{
		Version:  obs.SnapshotVersion,
		Proc:     "drain",
		PID:      os.Getpid(),
		Started:  now.Add(-2 * time.Hour),
		At:       now,
		Interval: 24 * time.Hour,
		Stopped:  false,
		Net:      "ONLINE",
		NetSince: now.Add(-45 * time.Minute),
		Probes: struct{ OK, Fail int }{
			OK:   184,
			Fail: 2,
		},
		LastProbe: &obs.Probe{
			At:      now.Add(-3 * time.Second),
			OK:      true,
			Latency: 14 * time.Millisecond,
			Target:  "mesh-gateway.local:8080",
		},
		ProbeMS: probeLat,
		Flips: []obs.Flip{
			{
				At:     now.Add(-95 * time.Minute),
				From:   "ONLINE",
				To:     "OFFLINE",
				Reason: "satellite link fade / zero reachable beacons",
			},
			{
				At:     now.Add(-45 * time.Minute),
				From:   "OFFLINE",
				To:     "ONLINE",
				Reason: "LoRa relay mesh uplink handshake confirmed",
			},
		},
		Queue: &jobqueue.Stats{
			Queued:  2,
			Running: 1,
			Done:    34,
			Dead:    0,
		},
		Jobs: map[string]obs.JobCounts{
			"sync_outbox": {Claimed: 35, Done: 32, Retried: 2, Dead: 0},
			"deep_think":  {Claimed: 9, Done: 8, Retried: 1, Dead: 0},
		},
		JobLatency: map[string]obs.Latency{
			"sync_outbox": {N: 32, P50: 120 * time.Millisecond, P95: 340 * time.Millisecond, Max: 680 * time.Millisecond},
			"deep_think":  {N: 8, P50: 3800 * time.Millisecond, P95: 5200 * time.Millisecond, Max: 6100 * time.Millisecond},
		},
		Feed: []obs.FeedLine{
			{At: now.Add(-10 * time.Second), Kind: "ok", Text: "flushed 4 pending CRDT ops to mesh link"},
			{At: now.Add(-35 * time.Second), Kind: "info", Text: "replica handshake verified: field-tablet-alpha (HMAC-SHA256)"},
			{At: now.Add(-2 * time.Minute), Kind: "ok", Text: "consensus verified: state hash matches across 3 replicas"},
		},
	}

	agentSnap := obs.Snapshot{
		Version:  obs.SnapshotVersion,
		Proc:     "agent",
		PID:      os.Getpid() + 1,
		Started:  now.Add(-2 * time.Hour),
		At:       now,
		Interval: 24 * time.Hour,
		Stopped:  false,
		Tiers: map[string]int{
			"rules": 54,
			"model": 21,
		},
		TierLat: map[string]obs.Latency{
			"rules": {N: 54, P50: 3 * time.Millisecond, P95: 8 * time.Millisecond, Max: 12 * time.Millisecond},
			"model": {N: 21, P50: 1840 * time.Millisecond, P95: 3400 * time.Millisecond, Max: 4200 * time.Millisecond},
		},
		Decisions: []obs.Decision{
			{
				At:     now.Add(-4 * time.Second),
				Text:   "create task site-4 pump status",
				Tier:   "rules",
				Reason: "rule create-task",
				Total:  4 * time.Millisecond,
			},
			{
				At:     now.Add(-18 * time.Second),
				Text:   "can you tell me the tasks that found in this place",
				Tier:   "model",
				Reason: "llama3.2:3b offline query",
				Total:  3220 * time.Millisecond,
			},
			{
				At:     now.Add(-55 * time.Second),
				Text:   "list open tasks",
				Tier:   "rules",
				Reason: "rule list-tasks",
				Total:  2 * time.Millisecond,
			},
			{
				At:     now.Add(-3 * time.Minute),
				Text:   "analyze battery degradation curve for site 2",
				Tier:   "model",
				Reason: "llama3.2:3b offline model inference",
				Total:  4150 * time.Millisecond,
			},
		},
		Feed: []obs.FeedLine{
			{At: now.Add(-18 * time.Second), Kind: "ok", Text: "offline llama3.2:3b inference completed (3.22 s)"},
			{At: now.Add(-3 * time.Minute), Kind: "info", Text: "drafted AI copilot proposal for T6 (MPPT calibration)"},
		},
	}

	if err := obs.WriteSnapshot(filepath.Join(metricsDir, "drain.json"), drainSnap); err != nil {
		fmt.Fprintf(os.Stderr, "error writing drain snapshot: %v\n", err)
		os.Exit(1)
	}
	if err := obs.WriteSnapshot(filepath.Join(metricsDir, "agent.json"), agentSnap); err != nil {
		fmt.Fprintf(os.Stderr, "error writing agent snapshot: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Wrote metrics snapshots to", metricsDir)
}
