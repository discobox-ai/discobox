# syntax=docker/dockerfile:1.7

# Declared before the first FROM so it is a global build argument the runtime
# stage's FROM can expand; declared alongside that FROM it would belong to the
# builder stage and resolve to nothing.
ARG BASE_IMAGE=discobox-base:local

# Build natively on the host platform and cross-compile, so multi-arch image
# builds only emulate the (apt-based) runtime stage, not the Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS sandbox-agent-builder
ARG TARGETOS TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
COPY agentcreds ./agentcreds
COPY access ./access
COPY api ./api
COPY execstream ./execstream
COPY harness ./harness
COPY proxy ./proxy
COPY runcca ./runcca
COPY sandboxconfig ./sandboxconfig
COPY sandboxuser ./sandboxuser
COPY sandbox-agent ./sandbox-agent

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-sandbox-agent ./cmd/discobox-sandbox-agent \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-runc ./cmd/discobox-runc \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-ca-anchor ./cmd/discobox-ca-anchor \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-docker ./cmd/discobox-docker \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C access build -trimpath -ldflags="-s -w" -o /out/discobox-access ./cmd/discobox-access

# Built in its own layer so bumping the difftui version does not invalidate the
# sandbox-agent binaries above.
# Pinned to a main pseudo-version rather than `latest`: difftui carries no
# tags, so `latest` is main's tip anyway, but an unpinned version makes the
# build unreproducible and leaves this layer cached on a stale commit. Bump it
# to pull a newer main.
ARG DIFFTUI_VERSION=v0.0.0-20260808165440-27da2073239f
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    set -eux \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
        go install -trimpath -ldflags="-s -w" github.com/discobox-ai/difftui@${DIFFTUI_VERSION} \
    # `go install` refuses a GOBIN with cross-compilation and drops
    # cross-built binaries under GOPATH/bin/<goos>_<goarch> instead, so pick
    # the binary up from whichever of the two layouts was used.
    && mv "$(go env GOPATH)/bin/${TARGETOS}_${TARGETARCH}/difftui" /out/difftui \
        || mv "$(go env GOPATH)/bin/difftui" /out/difftui

# fresh (getfresh.dev), the terminal editor the launcher's tools picker opens.
# Its own layer for the same reason difftui has one, and pinned to a release
# tag rather than following latest: an unpinned version makes the build
# unreproducible and leaves this layer cached on whatever it first saw. Bump it
# to pull a newer release.
#
# The upstream install script is not used. It resolves the release itself,
# installs under $HOME and writes desktop files — none of which survives being
# copied into an image, and all of which is a network fetch this build would
# not be able to pin. The release's own statically linked musl tarball is the
# thing that does: one binary, no runtime, the same on every base.
ARG FRESH_VERSION=v0.4.10
RUN set -eux \
    && case "$TARGETARCH" in \
        amd64) fresh_target=x86_64-unknown-linux-musl ;; \
        arm64) fresh_target=aarch64-unknown-linux-musl ;; \
        *) echo "no fresh build for ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && archive="fresh-editor-${fresh_target}.tar.gz" \
    && curl -fsSL -o /tmp/fresh.tar.gz \
        "https://github.com/sinelaw/fresh/releases/download/${FRESH_VERSION}/${archive}" \
    && tar -xzf /tmp/fresh.tar.gz -C /tmp \
    && mv "/tmp/fresh-editor-${fresh_target}/fresh" /out/fresh \
    && rm -rf /tmp/fresh.tar.gz "/tmp/fresh-editor-${fresh_target}"

# Debian, Docker, and the trimmed systemd unit set come from the shared base
# image, which the pool image is also built FROM so a pool host that pulls both
# pulls those layers once. See base-image/Dockerfile.
#
# The install steps below are split into a layer per tool group, ordered
# stablest first. They used to be one 4GB RUN, which meant adding a single apt
# package re-pulled every language runtime on every pool host and in every
# harness image. A layer boundary costs nothing and bounds the blast radius of
# each edit to the tools it actually touched.
FROM ${BASE_IMAGE} AS sandbox-agent

ENV NIX_CONFIG="experimental-features = nix-command flakes"
ENV NIX_REMOTE=daemon
ENV PATH=/nix/var/nix/profiles/default/bin:/usr/local/go/bin:/usr/local/bin:${PATH}

# Load-bearing for the `curl | sh` installers below: without pipefail a failed
# download silently feeds an empty script to a shell that exits 0.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# The base image enables no Docker unit, because the pool agent runs dockerd
# outright and the sandbox socket-activates it. This is the sandbox's half of
# that choice.
RUN systemctl disable docker.service containerd.service \
    && systemctl enable docker.socket

# Debian's half of the toolchain: everything from the distribution, and the
# largest layer here. It changes only when this list does.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    set -eux \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        chromium \
        direnv \
        extrepo \
        jq \
        less \
        libnss3-tools \
        openssh-client \
        openssh-sftp-server \
        passwd \
        poppler-utils \
        procps \
        psmisc \
        python-is-python3 \
        python3 \
        python3-pip \
        python3-requests \
        python3-venv \
        python3-websockify \
        python3-xdg \
        ripgrep \
        scrot \
        shellcheck \
        socat \
        sqlite3 \
        sudo \
        unzip \
        util-linux \
        vim \
        x11vnc \
        x11-xserver-utils \
        xdotool \
        xserver-xorg-core \
        xserver-xorg-video-dummy \
        xterm \
        xvfb \
        openbox \
        pcmanfm \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Nix, and the second-largest layer. Its own step so the tool groups below can
# change without repulling it.
RUN set -eux; \
    unset NIX_REMOTE; \
    # Build-time only: qemu-user (emulated cross-platform builds) cannot load
    # seccomp BPF programs, so disable nix syscall filtering for the install
    # steps without persisting it into the image's /etc/nix/nix.conf.
    export NIX_CONFIG="filter-syscalls = false"; \
    curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix \
        | sh -s -- install linux --extra-conf "sandbox = false" --init none --no-confirm; \
    install -D -m 0644 /nix/var/nix/profiles/default/lib/systemd/system/nix-daemon.service /etc/systemd/system/nix-daemon.service; \
    install -D -m 0644 /nix/var/nix/profiles/default/lib/systemd/system/nix-daemon.socket /etc/systemd/system/nix-daemon.socket; \
    # The shipped unit points at a man page this image has no man pages for.
    sed -i 's|^Documentation=man:nix-daemon https://nixos.org/manual$|Documentation=https://nixos.org/manual|' /etc/systemd/system/nix-daemon.service; \
    systemctl enable nix-daemon.socket; \
    nix profile install nixpkgs#devenv nixpkgs#bashInteractive; \
    devenv --version; \
    # ADR 0085: the stamp that guards the seed names the store it was cut
    # from, and a pool cache that already holds another build's store merges
    # this one in instead of overwriting the database that pool has been
    # writing to. Both need facts only the build knows, recorded beside the
    # seed rather than inside it: the store's identity -- a hash of its own
    # content hashes, so an identical rebuild re-seeds nobody -- the /nix path
    # of the nix-store binary this store delivers, and a dump of the database
    # rows that say which paths it registered.
    install -d -m 0755 /usr/local/lib/discobox; \
    { \
        echo "SEED_ID=$(LC_ALL=C ls -1 /nix/store | sha256sum | cut -d' ' -f1)"; \
        # The profile's store path plus the name, not `readlink -f` of the
        # binary: nix is one multi-call executable that dispatches on
        # argv[0], so resolving the link lands on `bin/nix` and loses the
        # nix-store personality along with `--load-db`.
        echo "SEED_NIX_STORE=$(readlink -f /nix/var/nix/profiles/default)/bin/nix-store"; \
    } > /usr/local/lib/discobox/nix-seed.env; \
    nix-store --dump-db > /usr/local/lib/discobox/nix-db.dump; \
    # ADR 0075: the store ships aside as a seed and /nix is left empty, so the
    # pool-shared cache volume can bind over it without hiding image content.
    # discobox-nix-seed.service copies it back on first use. The mv must stay
    # in this layer -- moving it in a later one copies the whole store up into
    # that layer and doubles the image instead of renaming a directory.
    mv /nix /usr/local/lib/discobox/nix; \
    mkdir -m 0755 /nix

RUN rustup --version >/dev/null 2>&1 \
    || curl --proto "=https" --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain none

# Third-party apt repositories. Separate from Debian's layer above because
# these track upstream release cadences and invalidate far more often.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    set -eux \
    && curl -fsSL https://deb.nodesource.com/setup_25.x | bash - \
    && sed -i 's|http://|https://|g' /etc/apt/sources.list.d/nodesource.list 2>/dev/null || true \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && extrepo enable mise \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        gh \
        mise \
        nodejs \
    && rm -rf /var/lib/apt/lists/*

# npm's cache is build-time scratch; leaving it behind put 100MB of it in every
# harness image, which repeats this trick to install its own CLI.
RUN set -eux \
    && npm install -g pnpm \
    && rm -rf /root/.npm

# Unpinned upstream toolchains, last among the installers because each tracks
# its own release and would otherwise invalidate everything below it.
RUN set -eux \
    && GO_VERSION="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)" \
    && curl -fsSL "https://go.dev/dl/${GO_VERSION}.linux-$(dpkg --print-architecture).tar.gz" | tar -C /usr/local -xz

RUN set -eux \
    && curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin sh \
    && curl -fsSL https://bun.sh/install | BUN_INSTALL=/usr/local bash

# Build the complete system trust store into the image, so the boot path only
# has to add this sandbox's MITM CA to it rather than rebuilding all 152
# certificates to add one (see runcca.MaterializeTrustStore). Last of the
# install steps so it sees the final ca-certificates package.
#
# It is built at /opt rather than /etc/ssl/certs, and by a redirected copy of
# update-ca-certificates rather than the tool itself, because a build running
# inside a Discobox sandbox has /etc/ssl/certs bind-mounted over by the runc
# wrapper: writes there go to the mount and vanish when the layer commits, which
# is why the image used to ship an empty store. LOCALCERTSDIR is pointed away so
# no build-time anchor is baked in; anchors belong to the sandbox that boots, and
# it adds its own.
RUN set -eux \
    && mkdir -p /opt/discobox/ca-certificates \
    && sed -e 's|^ETCCERTSDIR=.*|ETCCERTSDIR=/opt/discobox/ca-certificates|' \
           -e 's|^LOCALCERTSDIR=.*|LOCALCERTSDIR=/nonexistent|' \
           /usr/sbin/update-ca-certificates > /tmp/discobox-prebuild-ca-store \
    && chmod +x /tmp/discobox-prebuild-ca-store \
    && /tmp/discobox-prebuild-ca-store --fresh \
    && rm -f /tmp/discobox-prebuild-ca-store \
    && test -s /opt/discobox/ca-certificates/ca-certificates.crt

COPY sandbox-agent/image/profile.d/sandbox-dev-tools.sh /etc/profile.d/sandbox-dev-tools.sh
COPY sandbox-agent/image/profile.d/sandbox-direnv.sh /etc/profile.d/sandbox-direnv.sh
COPY sandbox-agent/image/systemd/discobox-sandbox-agent.service /etc/systemd/system/discobox-sandbox-agent.service
COPY sandbox-agent/image/systemd/discobox-proxy-bridge.service /etc/systemd/system/discobox-proxy-bridge.service
COPY sandbox-agent/image/systemd/discobox-buildkit-bridge.service /etc/systemd/system/discobox-buildkit-bridge.service
COPY sandbox-agent/image/systemd/discobox-trust-ca.service /etc/systemd/system/discobox-trust-ca.service
COPY sandbox-agent/image/systemd/discobox-render-proxy-env.service /etc/systemd/system/discobox-render-proxy-env.service
COPY sandbox-agent/image/systemd/containerd.service.d/runc-ca.conf /etc/systemd/system/containerd.service.d/runc-ca.conf
COPY sandbox-agent/image/systemd/docker.service.d/runc-ca.conf /etc/systemd/system/docker.service.d/runc-ca.conf
COPY sandbox-agent/image/systemd/docker.service.d/proxy.conf /etc/systemd/system/docker.service.d/proxy.conf
COPY sandbox-agent/image/systemd/docker.service.d/proxy-bridge.conf /etc/systemd/system/docker.service.d/proxy-bridge.conf
COPY sandbox-agent/image/systemd/nix-daemon.service.d/proxy.conf /etc/systemd/system/nix-daemon.service.d/proxy.conf
COPY sandbox-agent/image/systemd/nix-daemon.service.d/seed.conf /etc/systemd/system/nix-daemon.service.d/seed.conf
COPY sandbox-agent/image/systemd/nix-daemon.socket.d/seed.conf /etc/systemd/system/nix-daemon.socket.d/seed.conf
COPY sandbox-agent/image/systemd/discobox-nix-seed.service /etc/systemd/system/discobox-nix-seed.service
COPY sandbox-agent/image/systemd/discobox-proxy-bridge-docker.service /etc/systemd/system/discobox-proxy-bridge-docker.service
COPY sandbox-agent/image/daemon.json /etc/docker/daemon.json
COPY sandbox-agent/image/systemd/journald-console.conf /etc/systemd/journald.conf.d/discobox-console.conf
COPY sandbox-agent/image/systemd/x11-display.socket /etc/systemd/system/x11-display.socket
COPY sandbox-agent/image/systemd/x11-display.service /etc/systemd/system/x11-display.service
COPY sandbox-agent/image/systemd/x11vnc.socket /etc/systemd/system/x11vnc.socket
COPY sandbox-agent/image/systemd/x11vnc@.service /etc/systemd/system/x11vnc@.service
COPY sandbox-agent/image/systemd/websockify-proxy.socket /etc/systemd/system/websockify-proxy.socket
COPY sandbox-agent/image/systemd/websockify-proxy.service /etc/systemd/system/websockify-proxy.service
COPY sandbox-agent/image/systemd/websockify@.service /etc/systemd/system/websockify@.service
COPY sandbox-agent/image/systemd/xvfb.service /etc/systemd/system/xvfb.service
COPY sandbox-agent/image/systemd/openbox@.service /etc/systemd/system/openbox@.service
COPY sandbox-agent/image/xorg-dummy.conf /etc/X11/xorg-dummy.conf
COPY sandbox-agent/image/discobox-vnc-websockify /usr/local/bin/discobox-vnc-websockify
COPY --chmod=0755 sandbox-agent/image/nix-seed /usr/local/libexec/discobox/nix-seed
# Installs as `nix`, `devenv` and friends on /usr/local/bin, which PATH reaches
# only while the store's own bin directories are still empty. Starts the seed
# unit, then execs the real binary. See ADR 0075.
COPY --chmod=0755 sandbox-agent/image/nix-shim /usr/local/lib/discobox/nix-shim
COPY --from=sandbox-agent-builder /out/discobox-sandbox-agent /usr/local/bin/discobox-sandbox-agent
# Installs as `runc` on a directory prepended to containerd's and dockerd's
# PATH, so every container either one starts has the MITM CA bundles and proxy
# env injected into its OCI spec. See the shared runcca package.
COPY --from=sandbox-agent-builder /out/discobox-runc /opt/discobox/bin/runc
COPY --from=sandbox-agent-builder /out/discobox-ca-anchor /usr/local/bin/discobox-ca-anchor
# Its own binary, not an argv[0] alias of the agent: the credential CLI is a
# client of a portable protocol and is meant to be liftable into another
# repository, so it must not be welded to this image's runtime (ADR 0031).
COPY --from=sandbox-agent-builder /out/discobox-access /usr/local/bin/discobox-access
# And the skill that tells an agent the CLI is there. The image ships the
# skills for what the image installs, and the sandbox agent copies them into
# the harness's skill directories on the first primary launch, beside whatever
# the repository declares (ADR 0080). It lives in the module whose flags it
# documents, so changing them puts somebody in front of it.
COPY access/skills /usr/local/share/discobox/skills
# Installs as `docker`, ahead of the real CLI at /usr/bin/docker on PATH, to
# give `docker build` the pool-shared BuildKit cache. Non-build commands are
# exec'd straight through. See the sandbox-agent/dockercache package.
COPY --from=sandbox-agent-builder /out/discobox-docker /usr/local/bin/docker
COPY --from=sandbox-agent-builder /out/difftui /usr/local/bin/difftui
COPY --from=sandbox-agent-builder /out/fresh /usr/local/bin/fresh

RUN chmod 0755 /usr/local/bin/discobox-sandbox-agent \
    && chmod 0755 /opt/discobox/bin/runc \
    && chmod 0755 /usr/local/bin/discobox-ca-anchor \
    && chmod 0755 /usr/local/bin/docker \
    && chmod 0755 /usr/local/bin/difftui \
    && chmod 0755 /usr/local/bin/fresh \
    && chmod 0755 /usr/local/bin/discobox-vnc-websockify \
    && ln -sf /usr/local/bin/discobox-sandbox-agent /usr/local/bin/discobox-hook-publish \
    && for c in nix nix-build nix-env nix-shell nix-store devenv; do \
        ln -sf /usr/local/lib/discobox/nix-shim "/usr/local/bin/$c"; \
       done \
    # /etc/profile covers the login shells the sandbox launches, but not the
    # interactive shell a person starts by typing `bash` inside one. That shell
    # inherits the environment direnv already exported and would otherwise never
    # reload it on a `cd`, so hook it from the system-wide interactive rc too.
    && printf '\n# Discobox: direnv in the interactive shells /etc/profile does not reach.\nif [ -r /etc/profile.d/sandbox-direnv.sh ]; then\n  . /etc/profile.d/sandbox-direnv.sh\nfi\n' >> /etc/bash.bashrc \
    && mkdir -p /etc/discobox /run/discobox/agent-terminals /var/lib/docker /var/lib/discobox /workspace \
    && systemctl enable \
        discobox-sandbox-agent.service \
        discobox-proxy-bridge.service \
        discobox-buildkit-bridge.service \
        discobox-trust-ca.service \
        discobox-render-proxy-env.service \
        x11-display.socket \
        x11vnc.socket \
        websockify-proxy.socket

WORKDIR /workspace
# No VOLUME directives: the sandbox-agent init flow (PID 1) wires all persistent
# and cached paths from the primary volumes the worker mounts, per ADR 0007.
# Anonymous volumes here would only shadow those binds.
EXPOSE 6080
ENTRYPOINT ["/usr/local/bin/discobox-sandbox-agent", "init"]
CMD ["/sbin/init"]
