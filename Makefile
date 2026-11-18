# wiretuple — build/test tooling (GNU make, invoked as `make` or `make.cmd` on Windows).
# Zero external Go dependencies: the Go standard library only.
#
# Windows note: `-race` needs a C compiler. The environment maps gcc.cmd -> zig cc.

GO      ?= go
BIN_DIR ?= bin

.PHONY: test race bench build clean vet

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

bench:
	$(GO) test -run=^$$ -bench=. -benchmem ./benchmarks/
	$(GO) test -v -run TestBenchmarkTable ./benchmarks/

build:
	$(GO) build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/wiretuple ./cmd/wiretuple
	$(GO) build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/wiretuple-bench ./cmd/wiretuple-bench

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BIN_DIR)
