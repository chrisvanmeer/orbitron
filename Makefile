BINARY_NAME=orbitron
BUILD_DIR=bin

# Version stamp, overridable at build time (e.g. make build VERSION=v0.4.0)
VERSION?=dev
LDFLAGS=-ldflags="-w -s -X orbitron/internal/build.Version=$(VERSION)"

# Standard defaults for Linux (ARM64 matches Apple Silicon M1/OrbStack)
GOOS?=linux
GOARCH?=arm64

# In-repo Ansible collection sources + embedded seed target. The seed is the
# built collection artifact bundled into the binary so the daemon can seed the
# local cache out of the box (from `--install` and on every daemon start).
COLLECTION_SRC=ansible_collections/chrisvanmeer/orbitron
EMBED_DIR=internal/seed

# FORCE_SEED=1 bypasses the mtime heuristic and always regenerates the embedded
# seed from $(COLLECTION_SRC) (used by release builds, where file timestamps are
# unreliable). It fails hard when ansible-galaxy is missing.
FORCE_SEED?=0

.PHONY: all build build-linux-amd64 build-linux-arm64 build-all seed seed-regen verify-seed lint test clean

all: lint test build

# Rebuild the embedded collection seed artifact unconditionally. Requires
# ansible-galaxy; failing to find it is an error here because silently keeping
# the committed (possibly stale) artifact would ship a wrong version.
seed-regen:
	@command -v ansible-galaxy >/dev/null 2>&1 || { echo "  ✗ ansible-galaxy is not installed (required to regenerate the seed). Install ansible-core first." >&2; exit 1; }
	@echo "  ↻ Regenerating embedded Ansible collection seed..."; \
	rm -f $(EMBED_DIR)/collection.tar.gz; \
	rm -rf .collection-build; mkdir -p .collection-build; \
	ansible-galaxy collection build $(COLLECTION_SRC) --output-path .collection-build >/dev/null; \
	mv .collection-build/*.tar.gz $(EMBED_DIR)/collection.tar.gz; \
	rm -rf .collection-build; \
	printf '{"namespace":"%s","name":"%s","version":"%s"}\n' \
		`sed -n 's/^namespace:[[:space:]]*//p' $(COLLECTION_SRC)/galaxy.yml` \
		`sed -n 's/^name:[[:space:]]*//p' $(COLLECTION_SRC)/galaxy.yml` \
		`sed -n 's/^version:[[:space:]]*//p' $(COLLECTION_SRC)/galaxy.yml` \
		> $(EMBED_DIR)/meta.json

# Regenerate the embedded collection seed when the sources are newer than the
# committed artifact. Requires ansible-galaxy at that point; when it is missing
# the existing committed seed is kept so plain builds keep working. Use
# FORCE_SEED=1 to skip the mtime check (unreliable in CI after checkout).
seed:
	@mkdir -p $(EMBED_DIR)
	@if [ "$(FORCE_SEED)" = "1" ]; then \
		$(MAKE) seed-regen; \
	elif find $(COLLECTION_SRC) -type f -newer $(EMBED_DIR)/collection.tar.gz 2>/dev/null | grep -q .; then \
		if command -v ansible-galaxy >/dev/null 2>&1; then \
			$(MAKE) seed-regen; \
		else \
			echo "  ℹ Collection sources changed, but ansible-galaxy is not installed; keeping committed seed"; \
		fi; \
	else \
		echo "  ℹ Collection seed up to date"; \
	fi

# Fail when the collection sources and the embedded seed disagree on the
# version. Guarding every build (and CI) with this makes a galaxy.yml bump
# without a corresponding `make seed` an immediate, loud error.
verify-seed:
	@SRC=$$(sed -n 's/^version:[[:space:]]*//p' $(COLLECTION_SRC)/galaxy.yml); \
	SEED=$$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' $(EMBED_DIR)/meta.json); \
	if [ "$$SRC" != "$$SEED" ]; then \
		echo "  ✗ Seed version mismatch: galaxy.yml says $$SRC but $(EMBED_DIR)/meta.json says $$SEED. Run 'make seed FORCE_SEED=1' and commit the result." >&2; \
		exit 1; \
	fi; \
	echo "  ✔ Seed parity OK ($$SRC)"

# Standard build command (creates Linux ARM64 binary)
build: seed verify-seed
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) main.go

# Explicit Linux AMD64 build (for x86_64 production servers / CI)
build-linux-amd64: seed verify-seed
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 main.go

# Explicit Linux ARM64 build (for ARM servers & M1 OrbStack)
build-linux-arm64: seed verify-seed
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 main.go

# Build binaries for both Linux architectures at once
build-all: build-linux-amd64 build-linux-arm64

lint:
	@golangci-lint run ./... || go vet ./...

test:
	go test ./...

clean:
	rm -rf $(BUILD_DIR)
