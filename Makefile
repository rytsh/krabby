.DEFAULT_GOAL := build

VERSION := $(or $(VERSION),$(shell git describe --tags --first-parent --match "v*" 2> /dev/null || echo v0.0.0))
COMMIT  := $(shell git rev-parse --short HEAD 2> /dev/null || echo -)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build-ui
build-ui: ## Build the web UI into internal/server/dist (embedded by the Go build)
	cd _ui && pnpm install && pnpm build

.PHONY: build
build: ## Build the binary (run `make build-ui` first to embed the latest UI)
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/krabby ./cmd/krabby

.PHONY: all
all: build-ui build ## Build the UI then the binary

.PHONY: run
run: ## Run the server
	go run -ldflags '$(LDFLAGS)' ./cmd/krabby

.PHONY: test
test: ## Run tests
	go test -race -cover ./...

.PHONY: lint
lint: ## Run linters
	go vet ./...
	command -v golangci-lint > /dev/null && golangci-lint run ./... || true

.PHONY: build-container
build-container: build-ui ## Build the amd64 container image with a test tag
	GOOS=linux GOARCH=amd64 goreleaser build --snapshot --clean --single-target
	docker build -t krabby:test -f Dockerfile dist/krabby_linux_amd64_v1/

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-12s\033[0m %s\n", $$1, $$2}'
