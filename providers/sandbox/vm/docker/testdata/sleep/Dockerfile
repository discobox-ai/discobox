FROM debian:13-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
CMD ["sleep", "300"]
