# Auth Review Notes

- Do not store pool private keys centrally.
- Store bootstrap tokens only as hashes; make them short-lived and one-time use.
- Prefer short-lived, audience/scope-bound runtime tokens.
- Keep sandbox access issuer private keys encrypted at rest.
- Be explicit about key ownership: workload keys live with workloads; issuer keys live with the control plane.
- Token validation should check expiry, audience/scope, resource ownership, and revocation where applicable.
- Do not base authorization decisions on request-body fields; authorize from the authenticated principal and request attributes such as method, path parameters, query parameters, headers, and resource ownership loaded from those attributes. The only exception is pool bootstrap registration, which may redeem body-provided project/sandbox identity plus a one-time bootstrap token before a pool principal exists.
