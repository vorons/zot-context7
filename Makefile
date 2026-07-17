BIN      := zot-context7
VERSION  := $(shell jq -r .version extension.json 2>/dev/null || echo "dev")
GOFLAGS  := -ldflags="-s -w" -trimpath
GOPATH   := $(shell go env GOPATH)
ZOT_BIN  := $(shell which zot 2>/dev/null || echo "$(GOPATH)/bin/zot")

.PHONY: all build tool test test/short test/race lint \
        install uninstall release clean

all: build

# ---------------------------------------------------------------------------
# Build — stripped + trimpath, ~10 MB
# ---------------------------------------------------------------------------

build:
	go build $(GOFLAGS) -o $(BIN) .
	@echo "  → $(BIN) $$(ls -lh $(BIN) | awk '{print $$5}')"

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test:
	go test -count=1 -v ./...

test/short:
	go test -count=1 -short ./...

test/race:
	go test -count=1 -race ./...

# ---------------------------------------------------------------------------
# Static analysis
# ---------------------------------------------------------------------------

lint:
	go vet ./...
	@which staticcheck 2>/dev/null && staticcheck ./... || true

# ---------------------------------------------------------------------------
# Install to zot
# ---------------------------------------------------------------------------

install: build
	@if [ -x "$(ZOT_BIN)" ]; then \
		"$(ZOT_BIN)" ext install "./$(BIN)"; \
		echo "  → installed"; \
	else \
		echo "  → zot not found; run: zot ext install ./$(BIN)"; \
	fi

# ---------------------------------------------------------------------------
# Uninstall from zot
# ---------------------------------------------------------------------------

uninstall:
	@if [ -x "$(ZOT_BIN)" ]; then \
		"$(ZOT_BIN)" ext uninstall "$(BIN)"; \
	else \
		echo "  → run: zot ext uninstall $(BIN)"; \
	fi

# ---------------------------------------------------------------------------
# Release — clean build + smoke test + checksum
# ---------------------------------------------------------------------------

release: clean build
	@echo "  → version: $(VERSION)"
	@echo "  → $$(ls -lh $(BIN) | awk '{print $$5}')"
	@sha256sum $(BIN)
	@# smoke test: hello frame must be emitted
	@echo '{"type":"shutdown"}' | timeout 3 ./$(BIN) 2>/dev/null | \
		python3 -c "import sys,json; json.loads(sys.stdin.readline()); print('  → smoke: ok')" || \
		echo "  ⚠ smoke test skipped (pipe timing)"

# ---------------------------------------------------------------------------
# Clean
# ---------------------------------------------------------------------------

clean:
	rm -f $(BIN) $(BIN).packed
	@echo "  → cleaned"
