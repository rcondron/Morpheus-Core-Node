BINARY  := morcore
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/rcondron/Morpheus-Core-Node/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

.PHONY: build install test race lint fmt devnet release clean

## build: compile morcore for this machine into bin/
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/morcore

## install: install morcore into GOBIN
install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/morcore

## test: run all tests
test:
	go test ./...

## race: run all tests with the race detector
race:
	go test -race ./...

## lint: vet and check formatting
lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

## fmt: format the code
fmt:
	gofmt -w .

## devnet: run a local simulated marketplace (no real funds)
devnet:
	go run ./cmd/morcore-devnet

## release: cross-compile Linux, macOS and Windows (amd64 + arm64) into dist/
release:
	./scripts/release.sh $(VERSION)

clean:
	rm -rf bin dist
