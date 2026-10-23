# A plain Debian machine booting systemd: what a driver base is to the
# pool-agent bundle (ADR 26-10-23-551 §1). git is left out, so install has to
# provide it.
FROM public.ecr.aws/docker/library/debian:trixie-slim
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd systemd-sysv ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && ! command -v git
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
