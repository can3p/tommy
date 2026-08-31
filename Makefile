.PHONY: build test lint fmt run check

build: ## Build the tommy binary
	go build -o tommy .

test: ## Run the test suite with race detection and coverage
	go test -race -coverprofile=coverage.out ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Format all Go source files
	gofmt -w .

run: ## Build and run tommy
	go run . $(ARGS)

check: ## Run everything CI runs
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt'd:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	go vet ./...
	$(MAKE) lint
	go build ./...
	$(MAKE) test
