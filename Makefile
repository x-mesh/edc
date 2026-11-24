.PHONY: build install test check dist clean collector-build

VERSION ?= dev
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
DIST ?= dist
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
COLLECTOR_ARCH ?=

build:
	mkdir -p bin
	go build -ldflags "-X main.version=$(VERSION)" -o bin/edc ./cmd/edc

install:
	mkdir -p "$(BINDIR)"
	go build -ldflags "-X main.version=$(VERSION)" -o "$(BINDIR)/edc" ./cmd/edc

test:
	go test ./...

check:
# 포맷은 test보다 먼저 본다. 몇 분 걸리는 test를 다 돌린 뒤에 포맷 때문에 실패하지 않게 한다.
# 파일을 직접 넘기고 gofmt의 종료 코드도 본다. 문법 오류로 gofmt가 실패하면 목록이 비어도 여기서 멈춘다.
# -exec ... {} +는 경로를 셸 낱말로 나누지 않고 넘기며, gofmt가 실패하면 find도 실패를 돌려준다. .go 심볼릭 링크도 넘긴다.
	@unformatted="$$(find cmd internal -name '*.go' ! -type d -exec gofmt -l {} +)" || { echo "gofmt failed"; exit 1; }; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go test ./...
	go vet ./...
# release 빌드는 test 파일을 컴파일하지 않는다. 공용 test가 다른 OS의 분기 파일을 참조하면 그 OS의 job에서만 깨진다.
	GOOS=linux go vet ./...
	GOOS=darwin go vet ./...

collector-build:
	@test "$(COLLECTOR_ARCH)" = amd64 -o "$(COLLECTOR_ARCH)" = arm64 || { echo "COLLECTOR_ARCH must be amd64 or arm64" >&2; exit 2; }
	mkdir -p bin
	GOOS=linux GOARCH=$(COLLECTOR_ARCH) CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/edc-collector-linux-$(COLLECTOR_ARCH) ./cmd/edc-collector

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
	cp install.sh "$(DIST)/install.sh"
	@cd "$(DIST)" && if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum *.tar.gz install.sh > checksums.txt; \
	else \
		shasum -a 256 *.tar.gz install.sh > checksums.txt; \
	fi
	@ls -1 "$(DIST)"

clean:
	rm -f bin/edc
	rm -rf "$(DIST)"
