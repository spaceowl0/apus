# Apus build system
VERSION := 0.8.0
GO      ?= go
DIST    := dist

.PHONY: all build test test-short fuzz bench bench-native cross zip clean fmt vet

all: build

build:
	$(GO) build -trimpath -ldflags "-s -w" -o apus ./cmd/apus

fmt:
	gofmt -w cmd internal bench

vet:
	$(GO) vet ./...

# Full suite: unit tests, 30 programs x 3 backends, compile-error corpus, fuzzing, cross targets
test: vet
	$(GO) test ./... -timeout 600s

test-short:
	APUS_NO_NATIVE=1 $(GO) test ./... -timeout 300s

# Large differential fuzzing campaign (reference interpreter vs compiled vs native)
fuzz:
	APUS_FUZZ_N=20000 $(GO) test ./internal/e2e -run Fuzz -v -timeout 3600s

bench:
	$(GO) test ./bench -run xxx -bench . -benchmem -benchtime 500ms

bench-native: build
	python3 bench/native/run.py --runs 7

# Cross-compile the compiler itself for common platforms
cross:
	@mkdir -p $(DIST)
	GOOS=linux   GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/apus-linux-amd64   ./cmd/apus
	GOOS=linux   GOARCH=arm64 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/apus-linux-arm64   ./cmd/apus
	GOOS=darwin  GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/apus-darwin-amd64  ./cmd/apus
	GOOS=darwin  GOARCH=arm64 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/apus-darwin-arm64  ./cmd/apus
	GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/apus-windows-amd64.exe ./cmd/apus
	@ls -l $(DIST)

zip: clean
	cd .. && zip -r -q apus-$(VERSION)-src.zip $(notdir $(CURDIR)) -x '*/.git/*' '*/dist/*' '*/apus/apus' '*.bin'
	@echo "wrote ../apus-$(VERSION)-src.zip"

clean:
	rm -rf $(DIST) apus
