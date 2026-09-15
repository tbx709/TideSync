# TideSync - one binary, two roles, every common platform.
#
# `make` builds for the host, `make release` cross compiles everything and runs
# the checks. Override GO if the toolchain is not on PATH:
#   make GO=/usr/local/go/bin/go

GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: all build test vet fmt e2e release clean install help

all: build

## build: compile for the current platform into dist/
build:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/tidesync .

## test: run the unit and integration tests
test:
	$(GO) test ./...

## vet: static analysis for the host platform
vet:
	$(GO) vet ./...

## fmt: format all Go sources
fmt:
	gofmt -w main.go main_test.go cmd_*.go internal/

## e2e: build, then run the end to end test against a real agent
e2e: build
	./scripts/e2e-test.sh dist/tidesync

## release: cross compile every platform and verify the artifacts
release:
	./scripts/release.sh

## clean: remove build output
clean:
	rm -rf dist .work

## install: install the host binary into /usr/local/bin
install: build
	install -m 0755 dist/tidesync /usr/local/bin/tidesync

## help: list the available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
