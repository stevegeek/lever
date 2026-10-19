# Machine-specific values go in an untracked `local.mk` (see local.mk.example).
# It is included first, so its values win over the `?=` defaults below, as do
# the command line and the environment.
-include local.mk

# Where the host `lever` control-plane binary is installed (must be on PATH).
PREFIX ?= $(HOME)/.local/bin
# The instance tree, bind-mounted into the manager container.
LEVER_INSTANCE ?= $(HOME)/lever-instance

# Install the host `lever` binary and the host tools onto PATH.
.PHONY: install
install:
	@mkdir -p $(PREFIX)
	go build $(LEVER_BUILDVCS) -ldflags "$(LEVER_STAMP_LDFLAGS)" -o $(PREFIX)/lever ./cmd/lever
	@echo "installed $(PREFIX)/lever"; $(PREFIX)/lever version
	go build -ldflags "-X main.Version=$(LEVER_VERSION)" -o $(PREFIX)/lever-tool-github ./cmd/lever-tool-github
	@echo "installed $(PREFIX)/lever-tool-github"
	go build -ldflags "-X main.Version=$(LEVER_VERSION)" -o $(PREFIX)/lever-tool-whisper ./cmd/lever-tool-whisper
	@echo "installed $(PREFIX)/lever-tool-whisper"
	go build -ldflags "-X main.Version=$(LEVER_VERSION)" -o $(PREFIX)/lever-tool-tts ./cmd/lever-tool-tts
	@echo "installed $(PREFIX)/lever-tool-tts"

# The release version lives in internal/cli/root.go (CI greps that exact file).
# lever-agent cannot link internal/cli, so internal/agent.Version is stamped at
# build time.
LEVER_VERSION := $(shell sed -n 's/^const Version = "\(.*\)"/\1/p' internal/cli/root.go)
# The commit every lever binary reports (internal/buildstamp). It wins over Go's
# VCS stamp, which is wrong in a nested git worktree (see LEVER_BUILDVCS).
# Lazy (`=`), so git only runs for the targets that build.
LEVER_GIT_COMMIT = $(shell git rev-parse HEAD 2>/dev/null)
LEVER_STAMP_LDFLAGS = -X github.com/stevegeek/lever/internal/buildstamp.revision=$(LEVER_GIT_COMMIT) \
	-X github.com/stevegeek/lever/internal/buildstamp.modified=$(if $(LEVER_GIT_DIRTY),true,false)
LEVER_AGENT_LDFLAGS = -X github.com/stevegeek/lever/internal/agent.Version=$(LEVER_VERSION) $(LEVER_STAMP_LDFLAGS)
# lever-tool-db reports its version the same way (captool serverInfo).
LEVER_TOOL_DB_LDFLAGS = -X main.Version=$(LEVER_VERSION) $(LEVER_STAMP_LDFLAGS)
# The agent image's lever_version label: what `lever version` prints for this
# source, so `lever doctor` can compare the image's binaries with the host
# lever. Lazy (`=`), so git only runs for the targets that build an image.
LEVER_GIT_REV = $(shell git rev-parse --short=12 HEAD 2>/dev/null)
LEVER_GIT_DIRTY = $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
# Go's VCS stamping takes the first .git directory above the source, so in a
# git worktree (where .git is a file) nested inside another checkout it stamps
# that checkout's commit. There, build without it, so `go version -m` names no
# wrong commit; LEVER_STAMP_LDFLAGS carries the right one.
LEVER_BUILDVCS = $(if $(wildcard .git/HEAD),,-buildvcs=false)
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
		go build $(LEVER_BUILDVCS) -ldflags "$(LEVER_AGENT_LDFLAGS)" -o $(LEVER_INSTANCE)/vendor/bin/lever-agent ./cmd/lever-agent
	@file $(LEVER_INSTANCE)/vendor/bin/lever-agent

# The instance's lever-claude image build context.
LEVER_IMAGE_CTX ?= $(LEVER_INSTANCE)/image/lever-claude

# Stage the in-jail binaries and the scion pre-start hook into LEVER_IMAGE_CTX
# for the instance's build-lever-image.sh.
.PHONY: lever-image-bins
lever-image-bins:
	@mkdir -p $(LEVER_IMAGE_CTX)/bin $(LEVER_IMAGE_CTX)/scionhook
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build $(LEVER_BUILDVCS) -ldflags "$(LEVER_AGENT_LDFLAGS)" -o $(LEVER_IMAGE_CTX)/bin/lever-agent ./cmd/lever-agent
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build $(LEVER_BUILDVCS) -ldflags "$(LEVER_TOOL_DB_LDFLAGS)" -o $(LEVER_IMAGE_CTX)/bin/lever-tool-db ./cmd/lever-tool-db
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build $(LEVER_BUILDVCS) -ldflags "$(LEVER_STAMP_LDFLAGS)" -o $(LEVER_IMAGE_CTX)/bin/lever-manager ./cmd/lever-manager
	cp cmd/lever-agent/scionhook/pre-start $(LEVER_IMAGE_CTX)/scionhook/pre-start
	chmod +x $(LEVER_IMAGE_CTX)/scionhook/pre-start
	@file $(LEVER_IMAGE_CTX)/bin/lever-agent $(LEVER_IMAGE_CTX)/bin/lever-tool-db $(LEVER_IMAGE_CTX)/bin/lever-manager

# Build the generic lever-claude agent image in-repo, on the local
# scion-claude:<arch> base (see the build script).
FRAMEWORK_IMAGE_CTX := image/lever-claude
.PHONY: lever-image
lever-image: framework-image-stage
	LEVER_IMAGE_ARCH=$(LEVER_IMAGE_ARCH) LEVER_IMAGE_VERSION="$(LEVER_IMAGE_VERSION)" LEVER_JAIL_ABI="$(LEVER_JAIL_ABI)" bash $(FRAMEWORK_IMAGE_CTX)/build-lever-image.sh

# Stage the in-jail binaries and the pre-start hook into image/lever-claude, the
# build context of the lever layer both image flavors share.
.PHONY: framework-image-stage
framework-image-stage:
	@mkdir -p $(FRAMEWORK_IMAGE_CTX)/bin $(FRAMEWORK_IMAGE_CTX)/scionhook
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build $(LEVER_BUILDVCS) -ldflags "$(LEVER_AGENT_LDFLAGS)" -o $(FRAMEWORK_IMAGE_CTX)/bin/lever-agent ./cmd/lever-agent
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build $(LEVER_BUILDVCS) -ldflags "$(LEVER_TOOL_DB_LDFLAGS)" -o $(FRAMEWORK_IMAGE_CTX)/bin/lever-tool-db ./cmd/lever-tool-db
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 \
		go build $(LEVER_BUILDVCS) -ldflags "$(LEVER_STAMP_LDFLAGS)" -o $(FRAMEWORK_IMAGE_CTX)/bin/lever-manager ./cmd/lever-manager
	cp cmd/lever-agent/scionhook/pre-start $(FRAMEWORK_IMAGE_CTX)/scionhook/pre-start
	chmod +x $(FRAMEWORK_IMAGE_CTX)/scionhook/pre-start

# The slim agent image, lever.local/lever-claude-slim:<arch>: the same lever
# layer as lever-image, FROM lever's slim base (image/scion-slim) instead of
# scion-claude, so it needs no scion image build. The base gets sciontool and
# scion cross-compiled here from the scion source the jail runs: set
# SCION_VERSION to lever.yaml's scion.version, or SCION_SRC to the checkout
# scion.source names. The default is the pin the shipped examples use.
SCION_VERSION ?= 65531d7f
SCION_SRC ?=
# What the slim base records as its scion version: the pin, or the commit of
# the SCION_SRC checkout with -dirty for uncommitted changes (never its path,
# which is the host's).
SCION_IMAGE_VERSION = $(if $(SCION_SRC),$(or $(shell git -C "$(SCION_SRC)" describe --always --dirty --abbrev=12 2>/dev/null),unknown),$(SCION_VERSION))
SLIM_BASE_CTX := image/scion-slim
SCION_MODULE := github.com/GoogleCloudPlatform/scion
.PHONY: lever-image-slim
lever-image-slim: framework-image-stage slim-base-bins
	LEVER_IMAGE_FLAVOR=slim LEVER_IMAGE_ARCH=$(LEVER_IMAGE_ARCH) LEVER_IMAGE_VERSION="$(LEVER_IMAGE_VERSION)" LEVER_JAIL_ABI="$(LEVER_JAIL_ABI)" SCION_VERSION="$(SCION_IMAGE_VERSION)" bash $(FRAMEWORK_IMAGE_CTX)/build-lever-image.sh

# Cross-compile sciontool and scion for the slim base. go mod download runs
# outside this module so it cannot touch lever's go.mod or go.sum. Static
# (CGO_ENABLED=0) binaries need nothing from the image's libc; no_embed_web is
# how scion's own base builds them.
.PHONY: slim-base-bins
slim-base-bins:
	@mkdir -p $(SLIM_BASE_CTX)/bin
	@set -e; out="$(abspath $(SLIM_BASE_CTX))/bin"; src="$(SCION_SRC)"; \
	if [ -z "$$src" ]; then \
		dl=$$(cd / && go mod download -json $(SCION_MODULE)@$(SCION_VERSION) 2>&1) || true; \
		src=$$(printf '%s\n' "$$dl" | sed -n 's/.*"Dir": "\(.*\)".*/\1/p'); \
		[ -n "$$src" ] || printf '%s\n' "$$dl" | sed -n 's/.*"Error": "\(.*\)".*/go mod download: \1/p' >&2; \
	fi; \
	[ -n "$$src" ] && [ -f "$$src/go.mod" ] || { echo "no scion source for $(SCION_MODULE)@$(SCION_VERSION) (set SCION_VERSION or SCION_SRC)" >&2; exit 1; }; \
	echo "building sciontool and scion from $$src"; \
	cd "$$src" && \
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 go build -trimpath -tags no_embed_web \
		-ldflags "-s -w -X $(SCION_MODULE)/cmd/sciontool/commands.Version=$(SCION_IMAGE_VERSION) -X $(SCION_MODULE)/cmd/sciontool/commands.Commit=$(SCION_IMAGE_VERSION)" \
		-o "$$out/sciontool" ./cmd/sciontool && \
	GOOS=linux GOARCH=$(LEVER_IMAGE_ARCH) CGO_ENABLED=0 go build -trimpath -tags no_embed_web \
		-ldflags "-s -w -X $(SCION_MODULE)/pkg/version.Version=$(SCION_IMAGE_VERSION) -X $(SCION_MODULE)/pkg/version.Commit=$(SCION_IMAGE_VERSION)" \
		-o "$$out/scion" ./cmd/scion
	@file $(SLIM_BASE_CTX)/bin/sciontool $(SLIM_BASE_CTX)/bin/scion

# Check a built agent image against what lever and scion need in it
# (image/contract). IMAGE defaults to the slim image for LEVER_IMAGE_ARCH;
# LEVER_IMAGE_MAX_MB fails an image above that size. Needs docker (or
# CONTAINER_CLI=podman); runs the image with no network. The expected arch is
# the image tag's unless LEVER_IMAGE_ARCH is given on the command line or in
# the environment (the host default would wrongly fail a cross-arch image).
IMAGE ?= lever.local/lever-claude-slim:$(LEVER_IMAGE_ARCH)
.PHONY: image-contract
image-contract:
	LEVER_JAIL_ABI="$(LEVER_JAIL_ABI)" LEVER_IMAGE_ARCH=$(if $(filter command line environment,$(origin LEVER_IMAGE_ARCH)),$(LEVER_IMAGE_ARCH)) bash image/contract/check-image.sh "$(IMAGE)"

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

# The whole stack on the pinned scion (agent image, manager, worker, remote
# access, the experiment fence); see the script's header for what it needs.
.PHONY: test-lima-stack-e2e
test-lima-stack-e2e: install
	bash tools/test/lima-stack-e2e.sh

# Live e2e of the hub's proxy auth on a real Lima VM: migrates an instance an
# older lever brought up, binds and removes a contact while agents run,
# rotates the controller PAT, then brings a fresh instance up. Needs
# IMAGE_TAR (a docker archive of the agent image); see the script's header.
.PHONY: test-lima-hubauth-e2e
test-lima-hubauth-e2e: install
	bash tools/test/lima-hubauth-e2e.sh
