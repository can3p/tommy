.PHONY: build test lint fmt run check openapi

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

openapi: ## Regenerate the checked-in OpenAPI descriptions: the events API and each plugin's
	TOMMY_NO_UPDATE_CHECK=1 go run . openapi > docs/openapi.json
	@for p in as2 chat files hl7 mail push sms; do \
		echo "TOMMY_NO_UPDATE_CHECK=1 go run . openapi $$p > docs/openapi-$$p.json"; \
		TOMMY_NO_UPDATE_CHECK=1 go run . openapi $$p > docs/openapi-$$p.json; \
	done

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
