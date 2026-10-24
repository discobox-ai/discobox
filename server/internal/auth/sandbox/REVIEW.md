# Sandbox Auth Review Notes

- Do not store pool private keys centrally.
- Do not add bootstrap tokens: a box's maker enrolls its key over the channel it made the box with (ADR 26-10-24-953 §4).
- Prefer short-lived, audience/scope-bound runtime tokens.
- Keep sandbox access issuer private keys encrypted at rest.
- Be explicit about key ownership: workload keys live with workloads; issuer keys live with the control plane.
- Token validation should check expiry, audience/scope, resource ownership, and revocation where applicable.
- Do not base authorization decisions on request-body fields; authorize from the authenticated principal and request attributes such as method, path parameters, query parameters, headers, and resource ownership loaded from those attributes.
