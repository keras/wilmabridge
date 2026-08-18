BIN_DIR  := bin
BINARY   := $(BIN_DIR)/wilmabridge

DIST_DIR := dist

GO_BUILD := CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath

GO_SRCS := $(shell find cmd internal -name '*.go') go.mod go.sum

.PHONY: all build dist test clean

all: build

build: $(BINARY)

$(BINARY): $(GO_SRCS)
	$(GO_BUILD) -o $(BINARY) ./cmd/wilmabridge

# Build wilmabridge for release, written to dist/wilmabridge-linux-amd64.
# Used by the release workflow; safe to run locally too.
dist:
	mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=amd64 $(GO_BUILD) -o $(DIST_DIR)/wilmabridge-linux-amd64 ./cmd/wilmabridge

test:
	go test ./...

clean:
	rm -f $(BINARY)
	rm -rf $(DIST_DIR)
	go clean -cache
