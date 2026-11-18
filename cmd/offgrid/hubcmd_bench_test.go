package main

import (
	"io"
	"testing"
)

func BenchmarkHubCLINoArgsRouting(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cmdHub(nil, io.Discard, io.Discard)
	}
}

func BenchmarkHubCLIHelpRouting(b *testing.B) {
	args := []string{"help"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cmdHub(args, io.Discard, io.Discard)
	}
}

func BenchmarkHubGroupAddFlagParsing(b *testing.B) {
	args := []string{"group", "add", "-dir", "/tmp/hub", "-group", "crew", "-founder", "dev_0123456789abcdef0123456789abcdef"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Test routing error handling without valid key setup
		cmdHub(args, io.Discard, io.Discard)
	}
}
