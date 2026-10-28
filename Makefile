BINARY := k8s-triage-mcp
PKG := ./...

.PHONY: build test test-verbose cover vet fmt lint tidy-check check run clean kind-up kind-wait demo kind-down help

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

lint: ## run golangci-lint (install: brew install golangci-lint)
	golangci-lint run $(PKG)

tidy-check: ## fail if go.mod or go.sum are not tidy
	go mod tidy -diff

check: fmt tidy-check vet lint ## the same checks CI runs, plus tests with the race detector
	go test -race -count=1 $(PKG)

run: build ## run the server against the current kubeconfig context
	./bin/$(BINARY)

clean: ## remove build artifacts
	rm -rf bin coverage.out

kind-up: build ## create the kind demo cluster and apply every fixture
	scripts/demo.sh up

kind-wait: ## wait until every demo scenario is in its failure state
	scripts/demo.sh wait

demo: build ## show tool output for one scenario: make demo SCENARIO=orders
	scripts/demo.sh show $(SCENARIO)

kind-down: ## delete the kind demo cluster
	scripts/demo.sh down
