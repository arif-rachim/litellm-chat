BIN    := lchat
PREFIX ?= $(HOME)/.local

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || date +%Y.%m.%d)

.PHONY: build test install clean release

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN) .

release:
	./release.sh $(VERSION)

test:
	go vet ./...
	go test ./...

install: build
	install -Dm755 $(BIN) $(PREFIX)/bin/$(BIN)

clean:
	rm -f $(BIN)
