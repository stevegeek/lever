# Where the host `lever` control-plane binary is installed (must be on PATH).
PREFIX ?= $(HOME)/.local/bin
# The instance tree, bind-mounted into the manager container.
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

# Build + install both: host control plane (PATH) and the in-jail manager (instance tree).
.PHONY: all
all: install lever-manager-linux
