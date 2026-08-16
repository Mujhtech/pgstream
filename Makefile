# Build the web UI (embedded into the Go binary via go:embed)
webui:
	cd webui && bun install && bun run build

# Build the pgstream binary. Run `make webui` first (or at least once)
# so the embedded web UI assets are current.
build:
	go build -o pgstream ./cmd

# Build everything: web UI assets, then the binary embedding them.
all: webui build

test:
	go test ./...
	go vet ./...

.PHONY: webui build all test
