GO ?= go
PREFIX ?= /usr/local
VERSION ?= dev
DESTDIR ?= $(CURDIR)/build/root
LIBEXEC_ROOT := $(PREFIX)/libexec/noise
MODULE := github.com/noisefloor-oss/tasks
LDFLAGS := -X $(MODULE)/internal/buildinfo.Version=$(VERSION) -X $(MODULE)/internal/libexec.Root=$(LIBEXEC_ROOT)

.PHONY: build package check

build:
	mkdir -p build/bin build/libexec/noise
	$(GO) build -ldflags '$(LDFLAGS)' -o build/bin/noise ./cmd/noise
	$(GO) build -ldflags '$(LDFLAGS)' -o build/libexec/noise/noise-tasks ./cmd/noise-tasks
	$(GO) build -ldflags '$(LDFLAGS)' -o build/bin/noisetask ./cmd/noisetask
	$(GO) build -ldflags '$(LDFLAGS)' -o build/libexec/noise-setup ./cmd/noise-setup
	ln -f build/libexec/noise/noise-tasks build/bin/noise-tasks

package:
	GO='$(GO)' PREFIX='$(PREFIX)' VERSION='$(VERSION)' DESTDIR='$(DESTDIR)' sh scripts/package.sh

check:
	$(GO) build ./...
	$(GO) vet ./...
	$(GO) test ./...
