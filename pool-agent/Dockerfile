# syntax=docker/dockerfile:1.7

# Build natively on the host platform and cross-compile, so multi-arch image
# builds only emulate the (apt-based) runtime stage, not the Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS build
ARG TARGETOS TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
COPY api ./api
COPY controlplane ./controlplane
COPY harness ./harness
COPY id ./id
COPY layout ./layout
COPY proxy ./proxy
COPY sandboxconfig ./sandboxconfig
COPY gormdb ./gormdb
COPY pool-agent/go.mod pool-agent/go.sum ./pool-agent/
COPY pool-agent/cmd ./pool-agent/cmd
COPY pool-agent ./pool-agent
WORKDIR /src/pool-agent
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/discobox-pool-agent ./cmd/discobox-pool-agent

FROM debian:13-slim

ENV container=docker
ENV DISCOBOX_AGENT_PORT=3002

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        dbus \
        docker.io \
        git \
        systemd \
        systemd-sysv \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    # Trim systemd down to what a container actually needs. The host owns the
    # kernel, hardware, partitions, and network namespace, so anything that
    # manages those is dead weight or an outright failure at boot
    # (systemd-modules-load left this image booting degraded). udev IS masked
    # here, unlike the sandbox image, which needs it for nested Docker: the pool
    # agent only runs sandbox containers, and the udev daemon is absent today
    # solely because --no-install-recommends drops systemd's Recommends. Masking
    # keeps it from reappearing on a base-image bump. Periodic maintenance
    # (apt, dpkg, fstrim) is masked so the pool never mutates itself.
    && systemctl mask \
        apt-daily-upgrade.timer \
        apt-daily.timer \
        console-getty.service \
        dev-hugepages.mount \
        dpkg-db-backup.timer \
        e2scrub_all.timer \
        e2scrub_reap.service \
        fstrim.timer \
        getty.target \
        getty@.service \
        kmod-static-nodes.service \
        ldconfig.service \
        proc-sys-fs-binfmt_misc.mount \
        serial-getty@.service \
        sys-fs-fuse-connections.mount \
        systemd-ask-password-console.path \
        systemd-ask-password-console.service \
        systemd-ask-password-wall.path \
        systemd-ask-password-wall.service \
        systemd-battery-check.service \
        systemd-binfmt.service \
        systemd-boot-check-no-failures.service \
        systemd-bsod.service \
        systemd-confext.service \
        systemd-firstboot.service \
        systemd-growfs-root.service \
        systemd-hibernate-clear.service \
        systemd-hibernate-resume.service \
        systemd-hibernate.service \
        systemd-hostnamed.service \
        systemd-hostnamed.socket \
        systemd-hybrid-sleep.service \
        systemd-initctl.service \
        systemd-initctl.socket \
        systemd-journald-audit.socket \
        systemd-localed.service \
        systemd-logind.service \
        systemd-machine-id-commit.service \
        systemd-modules-load.service \
        systemd-networkd-wait-online.service \
        systemd-networkd.service \
        systemd-networkd.socket \
        systemd-pcrextend.socket \
        systemd-pcrlock.socket \
        systemd-pcrmachine.service \
        systemd-pcrphase-initrd.service \
        systemd-pcrphase-sysinit.service \
        systemd-pcrphase.service \
        systemd-pstore.service \
        systemd-quotacheck-root.service \
        systemd-random-seed.service \
        systemd-remount-fs.service \
        systemd-repart.service \
        systemd-rfkill.service \
        systemd-rfkill.socket \
        systemd-soft-reboot.service \
        systemd-suspend-then-hibernate.service \
        systemd-suspend.service \
        systemd-sysext.service \
        systemd-sysext.socket \
        systemd-timedated.service \
        systemd-tmpfiles-clean.timer \
        systemd-tpm2-setup-early.service \
        systemd-tpm2-setup.service \
        systemd-udev-settle.service \
        systemd-udev-trigger.service \
        systemd-udevd-control.socket \
        systemd-udevd-kernel.socket \
        systemd-udevd.service \
        systemd-update-utmp-runlevel.service \
        systemd-update-utmp.service \
        systemd-volatile-root.service

COPY --from=build /out/discobox-pool-agent /usr/local/bin/discobox-pool-agent
COPY pool-agent/image/systemd/discobox-proxy.service /etc/systemd/system/discobox-proxy.service

RUN mkdir -p /var/lib/docker /var/lib/discobox /var/lib/discobox/proxy /etc/discobox \
    && systemctl enable docker.service discobox-proxy.service

EXPOSE 3002
VOLUME ["/var/lib/docker", "/var/lib/discobox"]
HEALTHCHECK --interval=5s --timeout=2s --start-period=5s --retries=6 CMD curl -fsS "http://127.0.0.1:${DISCOBOX_AGENT_PORT:-3002}/healthz" >/dev/null || exit 1
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/bin/discobox-pool-agent"]
