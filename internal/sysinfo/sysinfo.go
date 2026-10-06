// Package sysinfo reads basic hardware facts so every benchmark result can say
// what machine produced it. A tokens/sec number without its hardware is
// meaningless, and honest numbers are the whole point of Day 1.
package sysinfo

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Info describes the machine.
type Info struct {
	CPU     string  `json:"cpu"`
	Threads int     `json:"threads"`
	MemGiB  float64 `json:"mem_gib"`
	OS      string  `json:"os"`
}

// Detect reads /proc and /etc/os-release. Anything it cannot read is left
// empty rather than guessed; on non-Linux systems only Threads and OS are set.
func Detect() Info {
	info := Info{
		Threads: runtime.NumCPU(),
		OS:      runtime.GOOS + "/" + runtime.GOARCH,
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		info.CPU = ParseCPUModel(string(b))
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		if kib := ParseMemTotalKiB(string(b)); kib > 0 {
			info.MemGiB = float64(kib) / 1024 / 1024
		}
	}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		if name := ParseOSName(string(b)); name != "" {
			info.OS = name
		}
	}
	return info
}

// ParseCPUModel returns the first "model name" from /proc/cpuinfo content.
func ParseCPUModel(cpuinfo string) string {
	for _, line := range strings.Split(cpuinfo, "\n") {
		if strings.HasPrefix(line, "model name") {
			if _, v, ok := strings.Cut(line, ":"); ok {
				return strings.Join(strings.Fields(v), " ")
			}
		}
	}
	return ""
}

// ParseMemTotalKiB returns MemTotal (in KiB) from /proc/meminfo content, or 0.
func ParseMemTotalKiB(meminfo string) int64 {
	for _, line := range strings.Split(meminfo, "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(strings.TrimPrefix(line, "MemTotal:"))
			if len(fields) > 0 {
				if n, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// ParseOSName returns PRETTY_NAME from /etc/os-release content.
func ParseOSName(osRelease string) string {
	for _, line := range strings.Split(osRelease, "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			v := strings.TrimPrefix(line, "PRETTY_NAME=")
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}
