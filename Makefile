VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 1.0.0)
LDFLAGS := -s -w -X github.com/johnswift/goelevenlabs/internal/server.Version=$(VERSION)
BIN := bin/goelevenlabs

.PHONY: build test install release clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) .

test:
	go test ./...

install: build
	install -m 0755 $(BIN) $(HOME)/.local/bin/goelevenlabs

# Static binaries for every platform (no cgo, no external runtime deps).
release:
	@for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/goelevenlabs-$$os-$$arch$$ext . ; \
	done

clean:
	rm -rf bin dist
