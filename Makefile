.PHONY: build test test-race vet fmt models bench

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
