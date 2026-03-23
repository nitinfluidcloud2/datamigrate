BINARY_NAME=datamigrate
BUILD_DIR=bin
GO=go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: all build clean test lint run deps install build-linux build-mac build-all

all: build

build:
	$(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/datamigrate

build-linux:
	GOOS=linux GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/datamigrate
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64"

build-mac:
	GOOS=darwin GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/datamigrate
	GOOS=darwin GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/datamigrate
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64"
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64"

build-all: build-linux build-mac
	@echo "All binaries built in $(BUILD_DIR)/"
	@ls -lh $(BUILD_DIR)/$(BINARY_NAME)-*

clean:
	rm -rf $(BUILD_DIR)
	$(GO) clean

test:
	$(GO) test -v ./...

test-cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

lint:
	golangci-lint run ./...

run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

deps:
	$(GO) mod tidy

install:
	$(GO) install $(LDFLAGS) ./cmd/datamigrate
