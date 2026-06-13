# syntax=docker/dockerfile:1

FROM debian:13

ARG TARGETARCH
ARG GO_VERSION=1.26.0
ARG NODE_VERSION=lts/*
ARG PYTHON_VERSION=3.13
ARG NVM_VERSION=0.40.5

ENV container=docker
ENV DEBIAN_FRONTEND=noninteractive
ENV NVM_DIR=/opt/nvm
ENV PYENV_ROOT=/opt/pyenv
ENV RUSTUP_HOME=/opt/rustup
ENV CARGO_HOME=/opt/cargo
ENV PATH=/opt/pyenv/shims:/opt/pyenv/bin:/usr/local/go/bin:/opt/cargo/bin:/root/.nix-profile/bin:/nix/var/nix/profiles/default/bin:${PATH}
ENV NIX_CONFIG="experimental-features = nix-command flakes"

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        apt-transport-https \
        bash \
        ca-certificates \
        curl \
        dbus \
        git \
        gnupg \
        iproute2 \
        jq \
        less \
        locales \
        openssh-client \
        procps \
        sudo \
        systemd \
        systemd-sysv \
        tar \
        unzip \
        vim \
        wget \
        xz-utils \
        zip \
        xfce4 \
        xfce4-terminal \
        build-essential \
        clang \
        cmake \
        gdb \
        make \
        pkg-config \
        libbz2-dev \
        libffi-dev \
        liblzma-dev \
        libncursesw5-dev \
        libreadline-dev \
        libsqlite3-dev \
        libssl-dev \
        llvm \
        tk-dev \
        zlib1g-dev \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && . /etc/os-release \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' "$VERSION_CODENAME" "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/docker.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        containerd.io \
        docker-buildx-plugin \
        docker-ce \
        docker-ce-cli \
        docker-compose-plugin \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && sed -i 's/^# *\(en_US.UTF-8 UTF-8\)/\1/' /etc/locale.gen \
    && locale-gen \
    && systemctl mask \
        dev-hugepages.mount \
        sys-fs-fuse-connections.mount \
        systemd-remount-fs.service \
        getty.target \
        console-getty.service \
    && systemctl enable docker.service

ENV LANG=en_US.UTF-8
ENV LC_ALL=en_US.UTF-8

RUN case "${TARGETARCH:-amd64}" in \
        amd64) go_arch=amd64 ;; \
        arm64) go_arch=arm64 ;; \
        *) echo "unsupported Go architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${go_arch}.tar.gz" -o /tmp/go.tgz \
    && rm -rf /usr/local/go \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz \
    && go version

RUN git clone --depth 1 https://github.com/pyenv/pyenv.git "$PYENV_ROOT" \
    && git clone --depth 1 https://github.com/pyenv/pyenv-update.git "$PYENV_ROOT/plugins/pyenv-update" \
    && git clone --depth 1 https://github.com/pyenv/pyenv-virtualenv.git "$PYENV_ROOT/plugins/pyenv-virtualenv" \
    && pyenv install -s "$PYTHON_VERSION" \
    && pyenv global "$PYTHON_VERSION" \
    && python --version \
    && pip install --no-cache-dir --upgrade pip setuptools wheel

RUN mkdir -p "$NVM_DIR" \
    && curl -fsSL "https://raw.githubusercontent.com/nvm-sh/nvm/v${NVM_VERSION}/install.sh" | PROFILE=/dev/null bash \
    && source "$NVM_DIR/nvm.sh" \
    && nvm install "$NODE_VERSION" \
    && nvm alias default "$NODE_VERSION" \
    && nvm use default \
    && node --version \
    && npm --version

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --profile default \
    && rustup --version \
    && rustc --version \
    && cargo --version

RUN curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix \
        | sh -s -- install linux --extra-conf "sandbox = false" --init none --no-confirm \
    && nix profile install nixpkgs#devenv nixpkgs#direnv nixpkgs#bashInteractive \
    && devenv --version \
    && direnv --version

RUN cat >/etc/profile.d/sandbox-dev-tools.sh <<'EOF'
export NVM_DIR=/opt/nvm
[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"
[ -s "$NVM_DIR/bash_completion" ] && . "$NVM_DIR/bash_completion"

export PYENV_ROOT=/opt/pyenv
export RUSTUP_HOME=/opt/rustup
export CARGO_HOME=/opt/cargo
export PATH="$PYENV_ROOT/bin:$PYENV_ROOT/shims:/usr/local/go/bin:$CARGO_HOME/bin:/root/.nix-profile/bin:/nix/var/nix/profiles/default/bin:$PATH"
command -v pyenv >/dev/null 2>&1 && eval "$(pyenv init - bash)"
command -v pyenv >/dev/null 2>&1 && eval "$(pyenv virtualenv-init -)"

if [ -e /root/.nix-profile/etc/profile.d/nix.sh ]; then
  . /root/.nix-profile/etc/profile.d/nix.sh
fi
EOF

RUN mkdir -p /var/lib/docker /var/lib/discobox /workspace

WORKDIR /workspace
VOLUME ["/var/lib/docker", "/var/lib/discobox", "/workspace"]
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
