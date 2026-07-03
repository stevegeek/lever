# Local overrides: put machine-specific paths in an untracked `local.mk` (e.g.
#   LEVER_INSTANCE := $(HOME)/ai/my-instance
# It is included first, so its values win over the `?=` defaults below. You can
# also override per-invocation on the command line (`make install LEVER_INSTANCE=...`)
# or via the environment — every var below uses `?=`. See local.mk.example.
-include local.mk

# Where the host `lever` control-plane binary is installed (must be on PATH).
PREFIX ?= $(HOME)/.local/bin
# The instance tree, bind-mounted into the manager container. Set this to your
# own instance directory (this default is a neutral placeholder).
LEVER_INSTANCE ?= $(HOME)/lever-instance

# Install the host `lever` binary (darwin/native) onto PATH — your everyday entry
# (`lever up` from anywhere inside an instance, or `lever up path/to/lever.yaml`).
.PHONY: install
install:
	@mkdir -p $(PREFIX)
	go build -o $(PREFIX)/lever ./cmd/lever
	@echo "installed $(PREFIX)/lever"; $(PREFIX)/lever version

# Cross-compile the in-jail orchestration binary for the OrbStack arm64 VM.
# Output goes to the instance tree, which is bind-mounted into the manager container.
.PHONY: lever-manager-linux
lever-manager-linux:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -o $(LEVER_INSTANCE)/vendor/bin/lever-manager ./cmd/lever-manager
	@file $(LEVER_INSTANCE)/vendor/bin/lever-manager

# Cross-compile the in-jail agent helper for the OrbStack arm64 VM. Used by the
# acceptance gate (run directly in the VM) and, baked into the
# agent image.
.PHONY: lever-agent-linux
lever-agent-linux:
	@mkdir -p $(LEVER_INSTANCE)/vendor/bin
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -o $(LEVER_INSTANCE)/vendor/bin/lever-agent ./cmd/lever-agent
	@file $(LEVER_INSTANCE)/vendor/bin/lever-agent

# The lever-claude image build context (where build-lever-image.sh runs docker
# build). Instance-specific — override to point at your image build dir.
LEVER_IMAGE_CTX ?= $(LEVER_INSTANCE)/image/lever-claude

# Cross-compile the agent helper + the reference db tool (linux/arm64) into the
# lever-claude image build context, and sync the scion pre-start hook there. Run
# before build-lever-image.sh so the Dockerfile can COPY them.
.PHONY: lever-image-bins
lever-image-bins:
	@mkdir -p $(LEVER_IMAGE_CTX)/bin $(LEVER_IMAGE_CTX)/scionhook
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -o $(LEVER_IMAGE_CTX)/bin/lever-agent ./cmd/lever-agent
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -o $(LEVER_IMAGE_CTX)/bin/lever-tool-db ./cmd/lever-tool-db
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -o $(LEVER_IMAGE_CTX)/bin/lever-manager ./cmd/lever-manager
	cp cmd/lever-agent/scionhook/pre-start $(LEVER_IMAGE_CTX)/scionhook/pre-start
	chmod +x $(LEVER_IMAGE_CTX)/scionhook/pre-start
	@file $(LEVER_IMAGE_CTX)/bin/lever-agent $(LEVER_IMAGE_CTX)/bin/lever-tool-db $(LEVER_IMAGE_CTX)/bin/lever-manager

# Build + install both: host control plane (PATH) and the in-jail manager (instance tree).
.PHONY: all
all: install lever-manager-linux

# Live api-key headline e2e (fake upstream; no real key). Rebuilds the host lever
# + image bins, bakes the image, then runs the e2e script. Needs OrbStack + podman.
.PHONY: test-apikey-e2e
test-apikey-e2e: install lever-image-bins
	bash $(LEVER_IMAGE_CTX)/build-lever-image.sh
	bash tools/test/apikey-e2e.sh

# Live lima-backend e2e: §19 `lever acceptance` six checks under both egress
# postures + guest port-forward suppression + idempotent closed re-bring-up +
# teardown, on a real Lima VM. Needs Lima >= 2.0 (brew install lima). The script
# builds the native lever-tool-db and the linux/<guestarch> lever-agent it needs.
.PHONY: test-lima-e2e
test-lima-e2e: install
	bash tools/test/lima-e2e.sh
