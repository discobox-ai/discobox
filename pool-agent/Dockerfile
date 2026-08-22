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
COPY layout ./layout
COPY proxy ./proxy
COPY runcca ./runcca
COPY sandboxconfig ./sandboxconfig
COPY sandboxuser ./sandboxuser
COPY pool-agent/go.mod pool-agent/go.sum ./pool-agent/
COPY pool-agent/cmd ./pool-agent/cmd
COPY pool-agent ./pool-agent
WORKDIR /src/pool-agent
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/discobox-pool-agent ./cmd/discobox-pool-agent \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/discobox-pool-runc ./cmd/discobox-pool-runc

FROM debian:13-slim

ENV container=docker
ENV DISCOBOX_AGENT_PORT=3002

# Marks the image as Discobox's to reclaim once no container uses it. See
# harness.ReclaimLabel and ADR 0040.
LABEL io.discobox.reclaimable.v1=true

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        dbus \
        docker.io \
        # The daemon package ships dockerd but no client. The client is here for
        # the pool host console (docs/adr/0051), which binds this daemon's socket
        # and is where an operator debugs a backend by hand.
        docker-cli \
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

# The pool-shared builder and its output registry (ADR 0044). Both are taken
# from their upstream images rather than fetched as release tarballs, so the
# binary set stays exactly what those projects ship and test together.
#
# BuildKit's CNI plugins are the `buildkit-cni-*` binaries in /usr/bin, not a
# /opt/cni/bin tree: `--oci-worker-net=bridge` generates its own CNI config
# against them, which is why no cni.json is needed here.
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkitd /usr/local/bin/buildkitd
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildctl /usr/local/bin/buildctl
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkit-runc /usr/bin/buildkit-runc
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-bridge /usr/bin/buildkit-cni-bridge
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-firewall /usr/bin/buildkit-cni-firewall
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-host-local /usr/bin/buildkit-cni-host-local
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-loopback /usr/bin/buildkit-cni-loopback
COPY --from=registry:2.8.3 /bin/registry /usr/local/bin/registry

COPY pool-agent/image/systemd/discobox-buildkitd.service /etc/systemd/system/discobox-buildkitd.service
COPY pool-agent/image/systemd/discobox-registry.service /etc/systemd/system/discobox-registry.service
COPY pool-agent/image/systemd/discobox-buildkit-mediator.service /etc/systemd/system/discobox-buildkit-mediator.service
COPY pool-agent/image/registry.yml /etc/discobox/registry.yml

# Installs as `runc` on the directory prepended to buildkitd's PATH, and is what
# --oci-worker-binary points at. It seeds the pool's MITM CA into every build
# step, so a user's Dockerfile needs no awareness of the proxy (ADR 0020/0044).
COPY --from=build /out/discobox-pool-runc /opt/discobox/bin/runc

RUN mkdir -p /var/lib/docker /var/lib/discobox /var/lib/discobox/proxy /etc/discobox /opt/discobox/bin \
    && systemctl enable docker.service discobox-proxy.service \
        discobox-buildkitd.service discobox-registry.service \
        discobox-buildkit-mediator.service

EXPOSE 3002
VOLUME ["/var/lib/docker", "/var/lib/discobox"]
HEALTHCHECK --interval=5s --timeout=2s --start-period=5s --retries=6 CMD curl -fsS "http://127.0.0.1:${DISCOBOX_AGENT_PORT:-3002}/healthz" >/dev/null || exit 1
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/bin/discobox-pool-agent"]
