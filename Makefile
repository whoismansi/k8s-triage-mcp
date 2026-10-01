BINARY := k8s-triage-mcp
PKG := ./...

.PHONY: build test lint vet fmt clean run help

help: ## show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## build the server binary into bin/
	go build -o bin/$(BINARY) ./cmd/$(BINARY)

test: ## run all tests
	go test $(PKG)

test-verbose: ## run all tests with output
	go test -v $(PKG)

cover: ## report test coverage for the triage logic
	go test -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

vet: ## run go vet
	go vet $(PKG)

fmt: ## format all Go source
	gofmt -s -w .

lint: fmt vet ## format, vet

check: lint test ## everything CI runs

run: build ## run the server against the current kubeconfig context
	./bin/$(BINARY)

clean: ## remove build artifacts
	rm -rf bin coverage.out
