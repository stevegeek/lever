# The generic Lever agent image: scion's stock claude harness + the lever-agent
# boot hook + the in-jail lever binaries. This is the minimal, instance-agnostic
# base every lever instance can run as-is. Instances that need extra runtimes or
# tools (a language toolchain, a KB CLI) extend it with their own Dockerfile
# `FROM lever-claude:<arch>` (images are tagged by arch, not :latest) — see
# docs-site "Building the agent image".
#
# The bin/ and scionhook/ build-context dirs are staged by `make lever-image`
# (which cross-compiles the binaries for the image's arch and copies the
# pre-start hook). Do not commit them; they are build output.
#
# SCION_BASE selects the arch-tagged base (scion-claude:arm64 / :amd64) so the
# two arches never clobber a shared :latest; build-lever-image.sh passes it.
ARG SCION_BASE=scion-claude:arm64
FROM ${SCION_BASE}

USER root

# The in-jail lever binaries (pre-built for the image arch by `make lever-image`).
#  - lever-agent:    enrols the agent's mTLS identity + registers brokered MCP tools (run by the hook)
#  - lever-manager:  the manager's orchestration CLI (agent start/list, msg, watch)
#  - lever-tool-db:  the reference first-party capability tool (optional; handy for demos)
COPY bin/lever-agent   /usr/local/bin/lever-agent
COPY bin/lever-tool-db /usr/local/bin/lever-tool-db
COPY bin/lever-manager /usr/local/bin/lever-manager
RUN chmod 0755 /usr/local/bin/lever-agent /usr/local/bin/lever-tool-db /usr/local/bin/lever-manager

# The lever scion pre-start hook, at the user-independent system dir. sciontool
# searches /etc/scion/hooks before $HOME/.scion/hooks, so this fires for every
# container and survives the per-agent /home/scion bind-mount. The hook runs
# `lever-agent boot`: generate the agent key IN the container (never leaves),
# enrol the mTLS identity with the broker, and register the capability MCP.
RUN mkdir -p /etc/scion/hooks/pre-start.d
COPY scionhook/pre-start /etc/scion/hooks/pre-start.d/10-lever-agent-boot
RUN chmod 0755 /etc/scion/hooks/pre-start.d/10-lever-agent-boot

# Point Claude Code at the agent's enrolled mTLS identity (written by the hook at
# boot) so it presents a client cert and trusts the broker CA when connecting to
# the broker's /mcp/<tool>/ gateway over mutual TLS. Real Claude Code env vars; a
# client cert is sent only when the server requests one, so these are inert for
# the Anthropic API connection itself.
# (Docker's SecretsUsedInArgOrEnv linter flags CLAUDE_CODE_CLIENT_KEY on the
# "KEY" name — a false positive: these values are FILE PATHS, not secrets. The
# key material is generated in the container at boot and never baked in. Don't
# "fix" the warning by removing these; the harness needs the paths.)
ENV CLAUDE_CODE_CLIENT_CERT=/home/scion/.lever-id/agent.crt
ENV CLAUDE_CODE_CLIENT_KEY=/home/scion/.lever-id/agent.key
ENV NODE_EXTRA_CA_CERTS=/home/scion/.lever-id/ca.crt

# Claude Code is baked at an EXPLICIT version. The scion-claude base installs
# it UNPINNED, so its copy is whatever was latest when the base was last
# rebuilt — silently stale. Bump this ARG deliberately (or pass
# --build-arg CLAUDE_CODE_VERSION=...), rebuild, `lever apply`; running
# containers pick it up when next recreated (manager: `lever stop && lever up`).
# The in-container auto-updater stays disabled (DISABLE_AUTOUPDATER below):
# updates happen by image rebuild, never at runtime.
ARG CLAUDE_CODE_VERSION=2.1.207
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
  && npm cache clean --force \
  && claude --version

# Claude is baked in and its global dir is root-owned, so the scion user can't
# self-update it; the auto-updater would only fail noisily every session.
ENV DISABLE_AUTOUPDATER=1

# Own the agent HOME as the runtime user. The jail forces ROOTLESS Docker (the
# isolated machine's seccomp blocks bpf → rootful runc fails), and the base image
# leaves /home/scion root-owned; under rootless userns mapping a root-owned home
# is unwritable by the scion user, which breaks scion's post-start hook. Owning it
# as scion (uid 1000) maps writable under rootless.
RUN chown -R scion:scion /home/scion

USER scion
