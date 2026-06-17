# Cross-compile the in-jail orchestration binary for the OrbStack arm64 VM.
# Output goes to the instance tree, which is bind-mounted into the manager container.
LEVER_INSTANCE ?= $(HOME)/lever-instance

.PHONY: lever-manager-linux
lever-manager-linux:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -o $(LEVER_INSTANCE)/vendor/bin/lever-manager ./cmd/lever-manager
	@file $(LEVER_INSTANCE)/vendor/bin/lever-manager
