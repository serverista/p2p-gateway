# Project settings
BINARY_NAME=p2p-gateway
MAIN=main.go

# Default target
.PHONY: all
all: build

## Build the binary
.PHONY: build
build:
	@echo "Building $(BINARY_NAME)..."
	go build -o cmd/p2p-gateway/$(BINARY_NAME) cmd/p2p-gateway/$(MAIN)

## Run the app (requires -api flag)
.PHONY: run
run: build
	@echo "Running $(BINARY_NAME)..."
	./cmd/p2p-gateway/$(BINARY_NAME) -api https://api.serverista.com

## Run tests
.PHONY: test
test:
	@echo "Running tests..."
	go test ./...

## Lint (basic vet)
.PHONY: lint
lint:
	@echo "Linting code..."
	go vet ./...

## Clean up build artifacts
.PHONY: clean
clean:
	@echo "Cleaning up..."
	rm -f cmd/p2p-gateway/$(BINARY_NAME)
