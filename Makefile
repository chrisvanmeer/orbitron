BINARY_NAME=orbitron
BUILD_DIR=bin

# Standard defaults for Linux (ARM64 matches Apple Silicon M1/OrbStack)
GOOS?=linux
GOARCH?=arm64

.PHONY: all build build-linux-amd64 build-linux-arm64 build-all lint test clean

all: lint test build

# Standard build command (creates Linux ARM64 binary)
build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="-w -s" -o $(BUILD_DIR)/$(BINARY_NAME) main.go

# Explicit Linux AMD64 build (for x86_64 production servers / CI)
build-linux-amd64:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 main.go

# Explicit Linux ARM64 build (for ARM servers & M1 OrbStack)
build-linux-arm64:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-w -s" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 main.go

# Build binaries for both Linux architectures at once
build-all: build-linux-amd64 build-linux-arm64

lint:
	@golangci-lint run ./... || go vet ./...

test:
	go test ./...

clean:
	rm -rf $(BUILD_DIR)
