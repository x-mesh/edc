.PHONY: build install test check clean

VERSION ?= dev
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

build:
	mkdir -p bin
	go build -ldflags "-X main.version=$(VERSION)" -o bin/edc ./cmd/edc

install:
	mkdir -p "$(BINDIR)"
	go build -ldflags "-X main.version=$(VERSION)" -o "$(BINDIR)/edc" ./cmd/edc

test:
	go test ./...

check:
	go test ./...
	go vet ./...

clean:
	rm -f bin/edc
