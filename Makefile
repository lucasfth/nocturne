GO ?= go
RELEASE_GO_VERSION := go1.26.9

.PHONY: go-build go-linux-amd64 go-toolchain-check

go-toolchain-check:
	@test "$$($(GO) env GOVERSION)" = "$(RELEASE_GO_VERSION)" || { echo "Release builds require $(RELEASE_GO_VERSION); set GO to an isolated verified compiler" >&2; exit 1; }
# Opt-in only: does not replace Python, install units, migrate data or start services.
go-build: go-toolchain-check
	mkdir -p bin
	cd go && CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o ../bin/nocturne ./cmd/nocturne

go-linux-amd64: go-toolchain-check
	mkdir -p bin
	cd go && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o ../bin/nocturne-linux-amd64 ./cmd/nocturne
