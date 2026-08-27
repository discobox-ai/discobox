# Architecture Decision Records

An ADR records a decision, the alternatives rejected, and why — at the time it
was made.

## When to write one

Only when either is true:

- A plausible alternative was rejected for a non-obvious reason.
- Something was deferred with a specific condition for revisiting it.

Otherwise skip the ADR and update the relevant `DESIGN.md`. Most changes do not
need one.

## Rules

- **Immutable.** Never edit an accepted ADR. Superseded by a new one that links
  back; mark the old one `Superseded by NNNN`.
- **Outside the drill-down hierarchy.** ADRs live here, never next to code.
  `DESIGN.md`/`REVIEW.md` are read root-down by agents on every task; ADRs are
  not, and must not be, because they are history rather than current state.
- **Not a substitute for `DESIGN.md`.** When the work lands, update the live
  design docs to describe what now exists. The ADR keeps the "why we didn't";
  `DESIGN.md` keeps the "what is".

## Status lifecycle

`Proposed` → `Accepted` → (`Superseded by NNNN`)

Use `Rejected` for decisions considered and declined; keep the file.

## Workflow

Nygard-style ADRs (see adr.github.io) combined with current-state design docs:

1. **Decide first.** Draft the ADR as `Proposed` and land it on its own,
   before implementation. Flipping it to `Accepted` is the decision gate —
   with review, the ADR's PR is where the review happens, and merge means
   accepted. Keep the status in the ADR header and the index table in sync.
2. **The accepted ADR is the spec** while implementation is in flight.
   `DESIGN.md` never describes in-progress or planned work — only what exists.
3. **DESIGN.md rides the code.** Each change that alters the architecture
   updates the affected `DESIGN.md` files in the same change, not as a
   follow-up pass. When the ADR's work fully lands, the live design docs
   describe the new state and the ADR keeps only the why.
4. **Plans are not documents.** Sequencing, checklists, and rollout order live
   in the task or branch driving the work; they are meant to go stale.
5. **Wrong decisions:** an `Accepted` ADR may still be amended while nothing
   has shipped against it. Once implementation has landed, supersede instead.

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-sandbox-origin-and-remote-source-push.md) | Sandbox origin and remote source push | Accepted |
| [0002](0002-harness-config-is-the-only-harness-concept.md) | Harness config is the only harness concept | Accepted |
| [0003](0003-promote-pool-to-a-first-class-primitive.md) | Promote pool to a first-class primitive | Accepted (§4 superseded by [0029](0029-sandboxes-have-no-per-sandbox-resource-requests.md)) |
| [0004](0004-user-namespaces-are-the-default-isolation.md) | User namespaces are the default isolation | Proposed |
| [0005](0005-kubernetes-backend-is-a-worker-driver.md) | Kubernetes backend is a worker driver | Proposed |
| [0006](0006-pool-is-the-runtime-host.md) | Pool is the runtime host; the worker resource is removed | Accepted |
| [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md) | Declarative sandbox volumes wired by the sandbox-agent | Proposed |
| [0008](0008-attach-stream-packages.md) | Attach stream is one protocol with two roles | Accepted |
| [0009](0009-previous-configure-secrets-are-prefixed-sentinels.md) | Previous configure secrets are offered as prefixed sentinels | Proposed |
| [0010](0010-deletes-are-hard-deletes.md) | Deletes are hard deletes | Proposed |
| [0011](0011-oauth-secrets-refresh-server-side-on-resolve.md) | OAuth secrets refresh server-side, on resolve | Proposed |
| [0012](0012-sandbox-config-is-three-attribute-owned-layers.md) | Sandbox config is three attribute-owned layers, merged by a shared library | Accepted |
| [0013](0013-local-linux-pools-use-libkrun-microvms.md) | Local Linux pools use libkrun microVMs with VSOCK and passt | Accepted |
| [0014](0014-disco-apply-pulls-sandbox-commits-via-cherry-pick.md) | `disco apply` pulls sandbox commits to the host via cherry-pick | Accepted |
| [0015](0015-nested-docker-builds-trust-the-mitm-proxy-via-nri.md) | Nested Docker builds and containers trust the MITM proxy via an NRI plugin | Superseded by [0020](0020-nested-docker-trust-is-injected-by-a-runc-wrapper.md) |
| [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) | Sandbox image upgrades are explicit, in-place, and digest-driven | Accepted (harnessless part superseded by [0032](0032-every-sandbox-has-a-harness-config-and-shell-is-the-built-in.md)) |
| [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md) | Orchestration is generation convergence; a resource has state and desired state | Accepted (§7 superseded for sandboxes by [0034](0034-sandbox-state-and-runtime-state-are-separate-fields.md)) |
| [0018](0018-disco-diff-resolves-its-base-inside-the-sandbox.md) | `disco diff` resolves its base inside the sandbox | Superseded by [0037](0037-drop-disco-diff-and-disco-status.md) |
| [0019](0019-one-server-per-data-directory-enforced-by-a-file-lock.md) | One server per data directory, enforced by an advisory file lock | Proposed |
| [0020](0020-nested-docker-trust-is-injected-by-a-runc-wrapper.md) | Nested Docker trust is injected by a runc wrapper, not an NRI plugin | Accepted |
| [0021](0021-upgrade-is-a-re-pin-and-preserves-power-state.md) | Upgrade is a desired-state re-pin, and replacing a container preserves its power state | Accepted |
| [0022](0022-sandbox-deletion-is-archive-then-confirmed-purge.md) | Sandbox deletion is archive, then confirmed purge | Accepted |
| [0023](0023-projects-are-created-by-copy-and-deleted-only-when-empty.md) | A project is created by copying an existing one, and deleted only when empty | Accepted |
| [0024](0024-ssh-is-a-control-plane-ingress-onto-execs.md) | SSH is a control-plane ingress onto execs, and forwarded TCP terminates inside the sandbox | Accepted (§1's TCP listener superseded by [0057](0057-ssh-reaches-the-server-only-through-the-cli-transport.md)) |
| [0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md) | The sandbox user is one contract, resolved inside the sandbox | Accepted (§6 launch-time re-lookup superseded by [0033](0033-user-resolution-is-one-layered-resolver-with-declared-gaps.md)) |
| [0026](0026-local-source-origin-is-bind-mounted-live-into-the-sandbox.md) | A local source's origin is bind-mounted live into the sandbox | Proposed |
| [0027](0027-harness-terminals-run-as-a-shells-typed-in-job.md) | Harness terminals run as a shell's typed-in job, not as the exec's own process | Accepted |
| [0028](0028-exec-log-transcripts-persist-as-compressed-sqlite-rows.md) | Exec/terminal transcripts persist as compressed sqlite rows, not tmpfs jsonl files | Accepted |
| [0029](0029-sandboxes-have-no-per-sandbox-resource-requests.md) | Sandboxes have no per-sandbox resource requests | Accepted |
| [0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md) | Pool-agent polls sandbox-agent status and pushes it to the control plane | Accepted |
| [0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md) | Agent credentials are a portable list/request/get protocol with pool-agent-minted ephemeral sentinels | Proposed |
| [0032](0032-every-sandbox-has-a-harness-config-and-shell-is-the-built-in.md) | Every sandbox has a harness config, and `shell` is the built-in one | Accepted (§2 `shell`'s image superseded by [0043](0043-shell-is-an-ordinary-harness-image.md); §1's fallback step by [0048](0048-a-sandbox-names-its-harness-or-the-project-does.md)) |
| [0033](0033-user-resolution-is-one-layered-resolver-with-declared-gaps.md) | User resolution is one layered resolver, and every gap is declared | Accepted |
| [0034](0034-sandbox-state-and-runtime-state-are-separate-fields.md) | Sandbox `state` and `runtime_state` are separate fields | Accepted |
| [0035](0035-repair-is-one-rebuild-intent-plus-a-start-instruction.md) | Repair is one rebuild intent, plus a start instruction | Accepted (§1 amended by [0064](0064-repair-rebuilds-on-the-current-image.md)) |
| [0036](0036-termpane-selection-is-a-mouse-only-cell-space-overlay.md) | Termpane selection is a mouse-only cell-space overlay | Accepted |
| [0037](0037-drop-disco-diff-and-disco-status.md) | Drop `disco diff` and `disco status` | Accepted |
| [0038](0038-terminal-identity-is-the-exec-id-terminals-revive-in-place.md) | Terminal identity is the exec id, and terminals revive in place | Accepted |
| [0039](0039-attach-waits-for-readiness-at-every-tier.md) | Attach waits for readiness at every tier | Accepted (progress-frame transport superseded by [0060](0060-provisioning-progress-is-a-recorded-phase-the-client-polls.md)) |
| [0040](0040-discobox-images-are-reclaimed-by-label-and-local-age.md) | Discobox images are reclaimed by label and local tag age | Accepted |
| [0041](0041-dev-hot-reload-is-watchnbuild.md) | Dev hot reload is watchnbuild, not Air | Proposed |
| [0042](0042-git-authorship-identity-is-a-first-class-sandbox-property.md) | Git authorship identity is a first-class sandbox property | Proposed |
| [0043](0043-shell-is-an-ordinary-harness-image.md) | `shell` is an ordinary harness image | Accepted |
| [0044](0044-builds-run-on-a-pool-shared-buildkit.md) | Builds run on a pool-shared BuildKit, bound to a sandbox by a mediator | Accepted |
| [0045](0045-a-directory-with-no-repository-is-delivered-by-push.md) | A directory with no repository is delivered by push | Accepted |
| [0046](0046-listening-ports-are-polled-and-probed-in-the-background.md) | Listening ports are discovered by a standing poller and probed for HTTP | Accepted |
| [0047](0047-local-base-images-resolve-through-a-per-sandbox-registry-namespace.md) | Local base images resolve through a per-sandbox registry namespace | Accepted |
| [0048](0048-a-sandbox-names-its-harness-or-the-project-does.md) | A sandbox names its harness, or the project does | Accepted (supersedes [0032](0032-every-sandbox-has-a-harness-config-and-shell-is-the-built-in.md) §1's fallback step) |
| [0049](0049-forwarded-ports-are-bound-near-their-number-and-held.md) | Forwarded ports are bound near their own number, and held once given | Accepted |
| [0050](0050-pool-build-state-is-not-sandbox-visible.md) | Pool build state lives outside the sandbox-visible cache | Accepted |
| [0051](0051-the-pool-console-attaches-through-the-driver.md) | The pool host console attaches through the driver's Docker client | Accepted |
| [0052](0052-iroh-is-an-optional-endpoint-scheme.md) | iroh is an optional endpoint scheme, and each hop names its endpoint package | Accepted (§4's "build the remaining targets now" superseded by [0053](0053-iroh-is-development-only-until-it-builds-everywhere.md)) |
| [0053](0053-iroh-is-development-only-until-it-builds-everywhere.md) | iroh is a development-only capability until it builds for macOS and Windows | Superseded by [0067](0067-iroh-ships-in-every-build.md) |
| [0054](0054-the-workspaces-columns-are-terminals-and-shells.md) | The workspace's two columns are terminals and shells, as the server records them | Accepted |
| [0055](0055-a-delivered-source-settles-before-its-sandbox-runs.md) | A delivered source settles before its sandbox runs | Proposed |
| [0056](0056-a-repository-declares-the-sources-it-is-worked-on-with.md) | A repository declares the sources it is worked on with | Proposed |
| [0057](0057-ssh-reaches-the-server-only-through-the-cli-transport.md) | SSH reaches the server only through the transport the API answers on | Accepted (supersedes [0024](0024-ssh-is-a-control-plane-ingress-onto-execs.md) §1's TCP listener) |
| [0058](0058-a-push-delivered-source-has-a-pool-side-origin.md) | A push-delivered source has a pool-side origin the client re-pushes into | Accepted |
| [0059](0059-a-rejected-swapped-credential-is-retried-once.md) | A rejected swapped credential is retried once, and the delivered file is restored | Accepted |
| [0060](0060-provisioning-progress-is-a-recorded-phase-the-client-polls.md) | Provisioning progress is a recorded phase, polled by the waiting client | Accepted |
| [0061](0061-the-client-facing-project-event-stream-is-removed.md) | The client-facing project event stream is removed | Accepted |
| [0062](0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md) | macOS pools run Virtualization.framework VMs, and the VM guest image is an independently released artifact | Accepted |
| [0063](0063-a-pool-agent-keeps-its-identity-key-and-registers-once.md) | A pool agent keeps its identity key, and registers once | Accepted |
| [0064](0064-repair-rebuilds-on-the-current-image.md) | Repair rebuilds on the current image | Accepted (amends [0035](0035-repair-is-one-rebuild-intent-plus-a-start-instruction.md) §1) |
| [0065](0065-the-cli-owns-its-pty-seam-and-windows-gets-conpty.md) | The CLI owns its pty seam, and Windows gets ConPTY | Accepted |
| [0066](0066-the-build-is-nix-plus-taskfile-and-github-actions-only-triggers-it.md) | The build is Nix plus the Taskfile, and GitHub Actions only triggers it | Accepted |
| [0067](0067-iroh-ships-in-every-build.md) | iroh ships in every build | Accepted (supersedes [0053](0053-iroh-is-development-only-until-it-builds-everywhere.md)) |
| [0068](0068-container-images-share-one-base-image.md) | Container images share one base image | Accepted |
| [0069](0069-staging-pool-images-is-a-condition.md) | Staging a pool's images is a condition, not a state | Accepted |
| [0070](0070-services-are-declared-execs-the-sandbox-starts-for-you.md) | Services are declared execs the sandbox starts for you | Accepted |
| [0071](0071-a-tool-session-is-an-exec-the-launcher-labeled.md) | A tool session is an exec the launcher labeled | Accepted |
| [0072](0072-a-repository-ships-skills-that-only-exist-in-a-sandbox.md) | A repository ships skills that only exist inside a sandbox | Proposed |
