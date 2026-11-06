# 26-11-06-628 — Attach waits for the outcome of blocking provisioning

- **Status**: Accepted by the owner's explicit decision on issue #749, 2026-11-06: "yes, waiting make sense"; the delivery was directed to continue with option 1.
- **Date**: 2026-11-06
- **Supersedes in part**: [0039](0039-attach-waits-for-readiness-at-every-tier.md), Tier 3 and "Timeouts nest, and measure stall rather than duration", for blocking sandbox provisioning only.

## Context

Cold provisioning fetches bundles and installs core, Node and the harness before
an exec can use them. A successful noninteractive package install can remain
silent beyond the former 90-second attach deadline. Output silence therefore
cannot decide whether this work failed. Returning a retry instruction breaks
the original create-and-attach intention while provisioning keeps running.

## Decision

An attach/start or command-creation request joins blocking provisioning until
readiness, a provisioning failure, or caller cancellation. It has no local
provisioning deadline. Cancellation ends that request without stopping the run
or creating a phantom exec. A running terminal bypasses the provisioning gate
when an agent restart provisions underneath it.

After readiness, terminal startup retains its separate bounded deadline. Pool
and provider startup budgets, source delivery policy, download transport stall
limits, and service-specific feature dependency gates remain unchanged.

The display watch stays independent of readiness. It describes the agent's
blocking feature and step, with
elapsed waiting time. Before the feature plan it describes bundle loading.
The spinner denotes a live client wait, not installer activity or a predicted
completion time. Failures remain errors on the original request.

## Alternatives rejected

A progress-refreshed attach stall timer still rejects healthy silent package
installs; bundle fetching also precedes feature progress. A larger fixed
cold-start budget merely moves the same retry failure to a slower machine or
network. The owner chose waiting for the provisioning outcome instead.
