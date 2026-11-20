
## Harness capture bootstrap

The resolved harness config carries its registered save-back definition and the
control plane's delivery baseline. Writing the sandbox document validates the
definition and places it on the image layer, with baseline revisions on the
runtime layer. The pool does not receive captured credentials; private capture
requests pass through to the agent under explicit control-plane scopes.

`buildSandboxDocument` owns this conversion and can fail before any manifest is
written. A capture-enabled harness requires a baseline for the same config ID,
with a positive revision and manifest digest. The serialized sandbox document
contains the effective config (not the raw layers); its harness retains the
config-owned baseline while project-owned additions remain protected.

## Machine agent endpoints

The machine runtime resolves the sandbox's service in pool scope. It keeps the
endpoint and any driver-issued edge credential in memory. Concurrent lookups
share a refresh, and the runtime refreshes before expiry and clears the endpoint
on machine lifecycle changes or a failed dial.

A credentialed endpoint uses verified TLS to the edge with HTTP/1.1. The
transport sets the URL's authority as the HTTP Host and uses the header named by
the driver. It leaves the sandbox-agent bearer token in Authorization.
