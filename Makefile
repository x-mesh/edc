.PHONY: build test check clean

VERSION ?= dev

build:
	mkdir -p bin
	go build -ldflags "-X main.version=$(VERSION)" -o bin/edc ./cmd/edc

test:
	go test ./...

check:
	go test ./...
	go vet ./...

clean:
	rm -f bin/edc
