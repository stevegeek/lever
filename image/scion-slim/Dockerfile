# The slim base for lever's agent image: what scion's runtime and lever's
# in-jail code need in an agent container, and nothing else. It stands in for
# scion's scion-claude image (about 5 GB with chromium, gcloud, gcsfuse, Go,
# kubectl, golangci-lint and bun) under image/lever-claude/Dockerfile, which
# adds the lever layer on top exactly as it does on scion-claude: Claude Code
# at its pin, tmux from trixie-backports, the lever binaries, the pre-start
# hook and /etc/claude-code. So Claude Code is installed once, there, and not
# here.
#
# `make lever-image-slim` stages bin/ (sciontool and scion, cross-compiled
# from the scion source lever.yaml pins) and builds this file, then the lever
# layer. bin/ is build output; do not commit it.
#
# What the agent container needs from this layer, and who needs it
# (image/contract/in-image.sh checks each of these against a built image):
#  - sciontool as the entrypoint (scion runs `sciontool init -- sh -c "tmux
#    new-session ..."`); it runs the pre-start hooks and the claude harness
#    provisioner (python3, stdlib only), and remaps the scion user with
#    usermod/groupmod or sed.
#  - scion: agents answer messages with `scion message`.
#  - user scion, uid and gid 1000, home /home/scion, login shell zsh: scion's
#    keep-id mapping and every lever probe run as that user.
#  - git: the agent's own work in /workspace (Debian trixie's 2.47 meets the
#    floor of the scion commit lever pins).
#  - tzdata, so a configured TZ takes effect (glibc silently falls back to
#    UTC for an unknown zone), and ca-certificates.
#  - procps: sciontool doctor and the agent use pgrep and ps.
#
# Left out on purpose: sudo (the agent gets no root in its container), gh
# (lever's agents reach GitHub through the broker's github tool), chromium, gcloud, gcsfuse, Go, kubectl, golangci-lint, bun, oh-my-zsh, delta
# and scion's init-firewall (lever enforces egress in the jail, not in the
# container). An instance that needs a toolchain adds it in its own
# Dockerfile FROM the finished image.
ARG NODE_IMAGE=node:24-trixie-slim
FROM ${NODE_IMAGE}

USER root
ARG DEBIAN_FRONTEND=noninteractive

# The node image's own user holds uid and gid 1000, which scion's user needs.
RUN set -eu; \
  . /etc/os-release; \
  test "$VERSION_CODENAME" = trixie; \
  u="$(getent passwd 1000 | cut -d: -f1)"; \
  if [ -n "$u" ]; then userdel -r "$u" 2>/dev/null || userdel "$u"; fi; \
  g="$(getent group 1000 | cut -d: -f1)"; \
  if [ -n "$g" ]; then groupdel "$g"; fi; \
  if getent passwd 1000 || getent group 1000; then exit 1; fi

RUN apt-get update \
  && apt-get install -y --no-install-recommends \
    ca-certificates git nano procps python3 tzdata zsh \
  && apt-get clean \
  && rm -rf /var/lib/apt/lists/*

COPY bin/sciontool /usr/local/bin/sciontool
COPY bin/scion     /usr/local/bin/scion
RUN chmod 0755 /usr/local/bin/sciontool /usr/local/bin/scion

# scion's base resolves whoami to the agent's scion identity inside an agent.
RUN printf '#!/bin/sh\nif [ -n "${SCION_AGENT_SLUG:-}" ] || [ -n "${SCION_AGENT_NAME:-}" ]; then\n    exec scion whoami "$@"\nfi\nexec /usr/bin/whoami "$@"\n' > /usr/local/bin/whoami \
  && chmod 0755 /usr/local/bin/whoami

# scion bind-mounts development binaries here when SCION_DEV_BINARIES is set,
# ahead of everything else on PATH.
RUN mkdir -p /opt/scion/bin

RUN useradd -m -s /bin/zsh -u 1000 -U scion \
  && test "$(id -u scion):$(id -g scion)" = 1000:1000

# The npm global prefix is the scion user's, as in scion's base, so the agent
# can npm install -g its own tools. The lever layer installs Claude Code into
# it as root.
RUN mkdir -p /home/scion/.scion /home/scion/.claude /workspace \
    /usr/local/share/npm-global/bin /usr/local/share/npm-global/lib/node_modules \
  && chown -R scion:scion /home/scion /workspace /usr/local/share/npm-global

ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global
ENV PATH=/opt/scion/bin:/usr/local/share/npm-global/bin:${PATH}
ENV LC_ALL=C.UTF-8
ENV EDITOR=nano
ENV VISUAL=nano
ENV SHELL=/bin/zsh
ENV DEVCONTAINER=true

WORKDIR /workspace

# The scion source sciontool and scion were built from (scion.version or the
# scion.source commit), for the operator; nothing reads it at runtime.
ARG SCION_VERSION=
LABEL scion_version="${SCION_VERSION}"

ENTRYPOINT ["sciontool", "init", "--"]
CMD ["bash"]
