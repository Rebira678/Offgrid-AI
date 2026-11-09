.PHONY: build test test-race vet fmt check models bench bench-system bench-models bench-quick bench-store bench-crdt demo demo-checkpoint1 partition-demo drain-demo top-demo release clock-demo lww-demo prove-merge orset-demo sync-demo converge-demo two-brains-demo ai-suggests-demo sim sim-quick web-demo web-check tamper-demo lint release-local good-first-issues fresh-test demo-gif launch-check wasm wasm-test doctor kill9-test phone-selftest fuzz-smoke vuln

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

# Day 27: edit the log by hand and watch it get caught (a careless edit, then a careful one). Needs python3.
tamper-demo: build
	scripts/tamper-demo.sh $(PAUSE)

# Day 28: the same lint CI runs (golangci-lint v2; config in .golangci.yml).
lint:
	golangci-lint run ./...

# Day 28: build the release binary exactly as the release workflow does, for this machine only.
#   make release-local VERSION=0.28.0
VERSION ?= 0.28.0
release-local:
	mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/offgrid ./cmd/offgrid
	@test "$$(./dist/offgrid version)" = "offgrid $(VERSION)" && echo "ok: ./dist/offgrid version says $(VERSION)"

# Day 28: open the starter issues on GitHub (needs gh, logged in). DRY=1 only prints them.
good-first-issues:
	scripts/create-good-first-issues.sh

# Day 29: a clean copy of this repo, built, tested and both demos run, timed (what a friend does). URL=... clones that instead.
fresh-test:
	scripts/fresh-clone-test.sh $(URL)

# Day 29: re-render the README's demo GIF from the demo's real output.
demo-gif: build
	PAUSE=0 scripts/converge-demo.sh > /tmp/offgrid-converge.out 2>&1 || true
	scripts/render-demo-gif.py /tmp/offgrid-converge.out docs/day-29-demo.gif

# Day 30: pre-flight checks before public launch and outreach.
launch-check:
	scripts/launch-check.sh

# Day 31: the merge packages (crdt, hlc) must stay buildable as WebAssembly, so a browser or phone can run the same rules.
#   make wasm        imports are standard library only, and they build for GOOS=js GOARCH=wasm
#   make wasm-test   also runs their tests as WebAssembly (needs node 18 or newer)
wasm:
	scripts/wasm-check.sh

wasm-test:
	scripts/wasm-check.sh --run

# Day 32: is this machine fit to run a node? Set DB=path to check a database too (add DEEP=1 for the hash chain).
doctor: build
	./bin/offgrid doctor $(if $(DB),-db $(DB)) $(if $(DEEP),-deep)

# Day 32: rehearse the power-pull test on this machine by killing the writer (N rounds). Not a power cut: see docs/day-32.md.
kill9-test: build
	scripts/real-device/kill9-test.sh $(or $(N),10)

# Day 33: rehearse the phone check in a desktop Chromium (needs: pip install playwright && playwright install chromium). Not a phone test.
phone-selftest: build
	OFFGRID_BIN=./bin/offgrid python3 scripts/real-device/phone-check-selftest.py

# Day 34: run every fuzz target for FUZZTIME each (default 10s; CI runs ten minutes nightly). Seeds already run in "make test".
FUZZTIME ?= 10s
fuzz-smoke:
	go test -run '^$$' -fuzz '^FuzzApplyRemoteOp$$'  -fuzztime $(FUZZTIME) ./internal/store/sqlstore
	go test -run '^$$' -fuzz '^FuzzSyncRequestBody$$' -fuzztime $(FUZZTIME) ./internal/syncer
	go test -run '^$$' -fuzz '^FuzzSyncAuth$$'        -fuzztime $(FUZZTIME) ./internal/syncer
	go test -run '^$$' -fuzz '^FuzzParseKeyFile$$'    -fuzztime $(FUZZTIME) ./internal/identity
	go test -run '^$$' -fuzz '^FuzzDeviceID$$'        -fuzztime $(FUZZTIME) ./internal/identity

# Day 34: known vulnerabilities in what Offgrid calls (needs network for the vulnerability database).
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
