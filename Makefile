.PHONY: build install test check dist clean

VERSION ?= dev
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
DIST ?= dist
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

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

# dist는 release가 올리는 asset을 만든다. 이름 규칙은 install.sh와 edc update가 함께 쓴다.
dist:
	rm -rf "$(DIST)"
	mkdir -p "$(DIST)"
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "build $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath \
			-ldflags "-s -w -X main.version=$(VERSION)" \
			-o "$(DIST)/edc" ./cmd/edc || exit 1; \
		tar -czf "$(DIST)/edc_$(VERSION)_$${os}_$${arch}.tar.gz" -C "$(DIST)" edc || exit 1; \
		rm -f "$(DIST)/edc"; \
	done
	@cd "$(DIST)" && if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum *.tar.gz > checksums.txt; \
	else \
		shasum -a 256 *.tar.gz > checksums.txt; \
	fi
	@ls -1 "$(DIST)"

clean:
	rm -f bin/edc
	rm -rf "$(DIST)"
