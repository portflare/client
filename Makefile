SHELL := /bin/bash

DIST_DIR ?= dist
BIN_DIR ?= $(DIST_DIR)/bin
RELEASE_DIR ?= $(DIST_DIR)/release
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/portflare/client/internal/buildinfo.Version=$(VERSION) \
	-X github.com/portflare/client/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/portflare/client/internal/buildinfo.Date=$(BUILD_DATE)

CLIENT_PKG := ./cmd/reverse-client

.PHONY: build release clean

build:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/reverse-client $(CLIENT_PKG)

release:
	mkdir -p $(BIN_DIR) $(RELEASE_DIR)
	for platform in $(PLATFORMS); do \
		goos="$${platform%/*}"; \
		goarch="$${platform#*/}"; \
		suffix=""; \
		if [ "$${goos}" = "windows" ]; then suffix=".exe"; fi; \
		client_bin="reverse-client-$${goos}-$${goarch}$${suffix}"; \
		archive_base="client-$${goos}-$${goarch}"; \
		CGO_ENABLED=0 GOOS="$${goos}" GOARCH="$${goarch}" go build -ldflags '$(LDFLAGS)' -o "$(BIN_DIR)/$${client_bin}" $(CLIENT_PKG); \
		rm -rf "$(RELEASE_DIR)/$${archive_base}"; \
		mkdir -p "$(RELEASE_DIR)/$${archive_base}"; \
		cp "$(BIN_DIR)/$${client_bin}" "$(RELEASE_DIR)/$${archive_base}/"; \
		cp README.md "$(RELEASE_DIR)/$${archive_base}/README.md"; \
		cp -R docs "$(RELEASE_DIR)/$${archive_base}/docs"; \
		cp -R examples "$(RELEASE_DIR)/$${archive_base}/examples"; \
		if [ "$${goos}" = "windows" ]; then \
			(cd $(RELEASE_DIR) && zip -r "$${archive_base}.zip" "$${archive_base}"); \
		else \
			tar -C $(RELEASE_DIR) -czf "$(RELEASE_DIR)/$${archive_base}.tar.gz" "$${archive_base}"; \
		fi; \
	done

clean:
	rm -rf $(DIST_DIR)
