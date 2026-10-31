.PHONY: build test test-race vet fmt check models bench bench-system bench-models bench-quick bench-store bench-crdt demo demo-checkpoint1 partition-demo drain-demo top-demo release clock-demo lww-demo prove-merge orset-demo sync-demo converge-demo two-brains-demo ai-suggests-demo sim sim-quick web-demo web-check

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

# Day 25: every benchmark, written to docs/ and into the README blocks between the bench:/perf: markers.
#   bench-system: store, sync (100 / 1,000 / 10,000 ops), queue drain and memory. No model. About 2 minutes.
#   bench-models: tokens/sec per model through Ollama. Skipped, with a message, when no Ollama server answers.
# Put the database files on the disk you care about with DIR=... (default: a temp directory).
MODELS ?= llama3.2:1b,llama3.2:3b
DIR ?=
bench: bench-system bench-models

bench-system: build
	./bin/offgrid perf $(if $(DIR),-dir $(DIR),) -md docs/perf.md -json docs/perf.json -svg docs/day-25-benchmarks.svg -readme README.md

bench-models: build
	@if ./bin/offgrid models >/dev/null 2>&1; then \
		./bin/offgrid bench -models $(MODELS) -md docs/benchmarks.md -json docs/benchmarks.json -readme README.md; \
	else \
		echo "bench-models: no Ollama server answered, so tokens/sec was NOT measured (start Ollama and rerun: make bench-models)"; \
	fi

# A 15-second pass (noisier numbers, nothing written to README).
bench-quick: build
	./bin/offgrid perf -quick

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

# Day 19: two real offgrid processes sync over localhost HTTP (needs openssl for the key).
sync-demo: build
	scripts/sync-demo.sh

# Day 20 / Checkpoint 2: two replicas, the network cut by the fault switch, edits on both sides, reconnect, hashes compared.
# scripts/converge-clip.sh (needs tmux) is the same scenario as a split-screen clip.
converge-demo: build
	scripts/converge-demo.sh

# Day 22: the slow brain. A hard question waits for a quiet laptop, is answered, and the answer syncs to a second device.
# STANDIN=0 uses your real Ollama with DEEP_MODEL (default qwen2.5:7b). BURN=0 skips the CPU burners.
two-brains-demo: build
	scripts/two-brains-demo.sh $(PAUSE)

# Day 23: a person edits a task on the phone while an AI proposes a different change on the laptop; sync; the person's edit survives.
ai-suggests-demo: build
	scripts/ai-suggests-demo.sh $(PAUSE)

# Day 24: deterministic simulation. Each seed is one random failure schedule (partitions, clock skew,
# crashes, lossy sync) against the real store. Replay a failing seed with: bin/offgrid sim -seed N
SEEDS ?= 1000
sim: build
	bin/offgrid sim -seeds $(SEEDS)

sim-quick: build
	bin/offgrid sim -seeds 100

# Day 26: the dashboard, on real data (a laptop and a phone that sync, an AI suggestion a person contradicted,
# a drain worker behind a scripted outage). SECS=60 keeps it up for a minute; PORT=8787 by default.
# The network is simulated and the model is a stand-in; the data and the dashboard are real.
web-demo: build
	scripts/web-demo.sh $(SECS)

# Same demo with no browser: fetches the page's data and fails if it is wrong.
web-check: build
	CHECK=1 scripts/web-demo.sh



