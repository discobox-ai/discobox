# Pool outbound configuration

`Intent` is one complete override. The server chooses API, YAML pool, global,
then direct, and resolves it once. `Resolved` is private boot/runtime material;
never return it through a public API or include it in diagnostics. Empty schemes
are direct and must never fall back to ambient environment. Additional CA
certificates are canonicalized by fingerprint and augment destination roots.

Managed OCI processes carry `DISCOBOX_PROXY_ENVIRONMENT=managed`; runcca consumes
it before trust injection and proxy retargeting. Unmarked processes keep the
ordinary enclosing sandbox behavior (ADR 26-11-05-814).

Explicitly selected inheritance recognizes an endpoint only when it matches
the enclosing sandbox's declared delivered proxy material. It records the same
logical forwarder as initial bootstrap, so Docker translates it through the
dynamic bridge and local VM drivers use their private relay. An unrelated
localhost endpoint and an explicitly configured endpoint are never reclassified.
