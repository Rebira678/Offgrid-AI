.PHONY: build test test-race vet fmt check models bench bench-store bench-crdt demo demo-checkpoint1 partition-demo drain-demo top-demo release clock-demo lww-demo prove-merge orset-demo

build:
	go build -o bin/offgrid ./cmd/offgrid

test:
	go test ./...

# The race detector needs a C compiler (sudo apt install build-essential).
test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

models: build
	./bin/offgrid models

# Full benchmark: writes docs/benchmarks.md and docs/benchmarks.json
bench: build
	./bin/offgrid bench -md docs/benchmarks.md -json docs/benchmarks.json

# Everything a contributor should run before pushing.
check: vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)
	go test ./...

# Append, rebuild, and CRDT fields speed of the SQLite store on this machine.
bench-store:
	go test -run '^$$' -bench 'Append|Rebuild/(1000|10000)|Fields/(1000|10000)' -benchtime 1x ./internal/store/sqlstore

# Benchmarks for in-memory CRDT merge and HLC logical clock operations.
bench-crdt:
	go test -run '^$$' -bench . -benchmem ./internal/crdt ./internal/hlc

# The no-network, no-model demo (pass PAUSE=2 for screen recording).
demo: build
	scripts/demo-offline.sh $(PAUSE)

# Checkpoint 1 demo: WiFi off, the agent keeps working, the queue fills.
demo-checkpoint1: build
	scripts/checkpoint1-demo.sh $(PAUSE)

# Day 11: flip a simulated partition on a timer and watch offgrid notice (local, safe to record).
partition-demo: build
	scripts/partition-demo.sh $(UP) $(DOWN)

# Day 12: kill -9 a running drain, restart, check nothing was lost or repeated.
drain-demo: build
	scripts/drain-demo.sh $(PAUSE)

# Day 13: the live one-screen view while a simulated outage comes and goes.
# Run in a terminal to see it live (record this); piped, it checks the screen tells the truth.
top-demo: build
	scripts/top-demo.sh $(SECS)

# Day 14: check everything, then tag v0.1 locally (pass PUSH=--push to publish)
release: build
	scripts/release-v0.1.sh $(PUSH)

# Day 15: why wall clocks cannot order edits, and the hybrid logical clock (a simulation).
clock-demo: build
	bin/offgrid clock demo

# Day 16: two people edit the same task while apart; how the merge decides (a simulation).
lww-demo: build
	bin/offgrid lww demo

# Day 17: property tests on the merge, then the same tests catching deliberately broken merges.
# Needs once: go get pgregory.net/rapid@v1.2.0
prove-merge:
	scripts/prove-merge.sh $(CHECKS)

# Day 18: delete a task while someone edits it; how the add-wins set decides (a simulation).
orset-demo: build
	bin/offgrid orset demo

