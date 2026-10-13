# Machine-specific values go in an untracked `local.mk` (see local.mk.example).
# It is included first, so its values win over the `?=` defaults below, as do
# the command line and the environment.
-include local.mk

# Where the host `lever` control-plane binary is installed (must be on PATH).
PREFIX ?= $(HOME)/.local/bin
# The instance tree, bind-mounted into the manager container.
LEVER_INSTANCE ?= $(HOME)/lever-instance

# Cross-compile the guest's login forwarder into
# internal/provision/loginfwd/prebuilt/ for go:embed, so the built lever needs
# no Go toolchain for remote access. The release workflow runs the same command.
.PHONY: loginfwd-prebuilt
loginfwd-prebuilt:
	go run ./internal/provision/loginfwd/genprebuilt

# Install the host `lever` binary and the host tools onto PATH.
.PHONY: install
install: loginfwd-prebuilt
	@mkdir -p $(PREFIX)
	go build -o $(PREFIX)/lever ./cmd/lever
	@echo "installed $(PREFIX)/lever"; $(PREFIX)/lever version
	go build -ldflags "-X main.Version=$(LEVER_VERSION)" -o $(PREFIX)/lever-tool-github ./cmd/lever-tool-github
	@echo "installed $(PREFIX)/lever-tool-github"
	go build -ldflags "-X main.Version=$(LEVER_VERSION)" -o $(PREFIX)/lever-tool-fizzy ./cmd/lever-tool-fizzy
	@echo "installed $(PREFIX)/lever-tool-fizzy"
	go build -ldflags "-X main.Version=$(LEVER_VERSION)" -o $(PREFIX)/lever-tool-whisper ./cmd/lever-tool-whisper
	@echo "installed $(PREFIX)/lever-tool-whisper"
	go build -ldflags "-X main.Version=$(LEVER_VERSION)" -o $(PREFIX)/lever-tool-tts ./cmd/lever-tool-tts
	@echo "installed $(PREFIX)/lever-tool-tts"

# The release version lives in internal/cli/root.go (CI greps that exact file).
# lever-agent cannot link internal/cli, so internal/agent.Version is stamped at
# build time.
LEVER_VERSION := $(shell sed -n 's/^const Version = "\(.*\)"/\1/p' internal/cli/root.go)
LEVER_AGENT_LDFLAGS := -X github.com/stevegeek/lever/internal/agent.Version=$(LEVER_VERSION)
# lever-tool-db reports its version the same way (captool serverInfo).
LEVER_TOOL_DB_LDFLAGS := -X main.Version=$(LEVER_VERSION)
# The agent image's lever_version label: what `lever version` prints for this
# source, so `lever doctor` can compare the image's binaries with the host
# lever. Lazy (`=`), so git only runs for the targets that build an image.
LEVER_GIT_REV = $(shell git rev-parse --short=12 HEAD 2>/dev/null)
LEVER_GIT_DIRTY = $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
LEVER_IMAGE_VERSION = $(LEVER_VERSION)$(if $(LEVER_GIT_REV), ($(LEVER_GIT_REV)$(LEVER_GIT_DIRTY)))
# The agent image's lever_jail_abi label (internal/jailabi), which `lever
# doctor` compares with the host lever. A test in internal/jailabi keeps this
# sed's line shape.
LEVER_JAIL_ABI := $(shell sed -n 's/^const JailABI = \([0-9]*\)$$/\1/p' internal/jailabi/jailabi.go)

# The jail's Linux arch; override for a cross build.
LEVER_IMAGE_ARCH ?= $(shell go env GOARCH)

# Cross-compile the in-jail agent helper for the acceptance gate (run directly
# in the VM).
.PHONY: lever-agent-linux
lever-agent-linux:
	@mkdir -p $(LEVER_INSTANCE)/vendor/bin
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build -ldflags "$(LEVER_AGENT_LDFLAGS)" -o $(LEVER_INSTANCE)/vendor/bin/lever-agent ./cmd/lever-agent
	@file $(LEVER_INSTANCE)/vendor/bin/lever-agent

# The instance's lever-claude image build context.
LEVER_IMAGE_CTX ?= $(LEVER_INSTANCE)/image/lever-claude

# Stage the in-jail binaries and the scion pre-start hook into LEVER_IMAGE_CTX
# for the instance's build-lever-image.sh.
.PHONY: lever-image-bins
lever-image-bins:
	@mkdir -p $(LEVER_IMAGE_CTX)/bin $(LEVER_IMAGE_CTX)/scionhook
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build -ldflags "$(LEVER_AGENT_LDFLAGS)" -o $(LEVER_IMAGE_CTX)/bin/lever-agent ./cmd/lever-agent
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build -ldflags "$(LEVER_TOOL_DB_LDFLAGS)" -o $(LEVER_IMAGE_CTX)/bin/lever-tool-db ./cmd/lever-tool-db
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build -o $(LEVER_IMAGE_CTX)/bin/lever-manager ./cmd/lever-manager
	cp cmd/lever-agent/scionhook/pre-start $(LEVER_IMAGE_CTX)/scionhook/pre-start
	chmod +x $(LEVER_IMAGE_CTX)/scionhook/pre-start
	@file $(LEVER_IMAGE_CTX)/bin/lever-agent $(LEVER_IMAGE_CTX)/bin/lever-tool-db $(LEVER_IMAGE_CTX)/bin/lever-manager

# Build the generic lever-claude agent image in-repo, on the local
# scion-claude:<arch> base (see the build script).
FRAMEWORK_IMAGE_CTX := image/lever-claude
.PHONY: lever-image
lever-image:
	@mkdir -p $(FRAMEWORK_IMAGE_CTX)/bin $(FRAMEWORK_IMAGE_CTX)/scionhook
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build -ldflags "$(LEVER_AGENT_LDFLAGS)" -o $(FRAMEWORK_IMAGE_CTX)/bin/lever-agent ./cmd/lever-agent
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build -ldflags "$(LEVER_TOOL_DB_LDFLAGS)" -o $(FRAMEWORK_IMAGE_CTX)/bin/lever-tool-db ./cmd/lever-tool-db
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build -o $(FRAMEWORK_IMAGE_CTX)/bin/lever-manager ./cmd/lever-manager
	cp cmd/lever-agent/scionhook/pre-start $(FRAMEWORK_IMAGE_CTX)/scionhook/pre-start
	chmod +x $(FRAMEWORK_IMAGE_CTX)/scionhook/pre-start
	LEVER_IMAGE_ARCH=$(LEVER_IMAGE_ARCH) LEVER_IMAGE_VERSION="$(LEVER_IMAGE_VERSION)" LEVER_JAIL_ABI="$(LEVER_JAIL_ABI)" bash $(FRAMEWORK_IMAGE_CTX)/build-lever-image.sh

# Print the lever_version label value, for an instance image build that sets
# the label in its own Dockerfile.
.PHONY: lever-image-version
lever-image-version:
	@echo "$(LEVER_IMAGE_VERSION)"

# Print the lever_jail_abi label value, for the same kind of image build.
.PHONY: lever-image-jail-abi
lever-image-jail-abi:
	@echo "$(LEVER_JAIL_ABI)"

# The in-jail binaries ship baked into the agent image, not in the tree.
.PHONY: all
all: install

# Tests behind the `integration` build tag. Each skips unless its own
# prerequisites are present.
.PHONY: test-integration
test-integration:
	go test -tags integration -timeout 40m ./...

# Live api-key e2e against a fake upstream (no real key). Needs OrbStack and
# podman.
.PHONY: test-apikey-e2e
test-apikey-e2e: install lever-image-bins
	LEVER_IMAGE_ARCH=$(LEVER_IMAGE_ARCH) LEVER_IMAGE_VERSION="$(LEVER_IMAGE_VERSION)" LEVER_JAIL_ABI="$(LEVER_JAIL_ABI)" bash $(LEVER_IMAGE_CTX)/build-lever-image.sh
	bash tools/test/apikey-e2e.sh

# Live e2e on a real Lima VM (Lima >= 2.0): acceptance checks under both
# egress postures, port-forward suppression, re-bring-up and teardown.
.PHONY: test-lima-e2e
test-lima-e2e: install
	bash tools/test/lima-e2e.sh
