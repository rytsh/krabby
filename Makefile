.DEFAULT_GOAL := build

VERSION := $(or $(VERSION),$(shell git describe --tags --first-parent --match "v*" 2> /dev/null || echo v0.0.0))
COMMIT  := $(shell git rev-parse --short HEAD 2> /dev/null || echo -)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build
build: ## Build the binary
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/krabby ./cmd/krabby

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

.PHONY: docker
docker: ## Build docker image
	docker build -t krabby:$(VERSION) .

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-12s\033[0m %s\n", $$1, $$2}'
