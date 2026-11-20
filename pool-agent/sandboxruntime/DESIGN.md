
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

## Managed skills

The runtime reads desired skills as the hosting pool and includes a
snapshot in runtime config for agents that advertise support. It checks
the latest revision at boot, resume and during convergence. A conditional
revision read keeps unchanged snapshots out of recurring response bodies.
The sandbox's applied skills revision drives retries independently of the
runtime-config revision.
