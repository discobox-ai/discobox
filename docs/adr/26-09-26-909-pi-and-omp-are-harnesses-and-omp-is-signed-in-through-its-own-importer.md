# 26-09-26-909 — Pi and Oh My Pi are harnesses, and omp is signed in through its own importer

- **Status**: Accepted
- **Date**: 2026-09-26

## Context

Discobox ships harnesses for Claude Code, Codex, and opencode. Two more
terminal agents are asked for: **Pi** (`@earendil-works/pi-coding-agent`, a
minimal, extensible agent by Mario Zechner) and **Oh My Pi** (`omp`,
`@oh-my-pi/pi-coding-agent`, a batteries-included fork of Pi that runs on
bun). Both reach every provider they bundle and let a user sign in to as many
as they like, the shape the opencode harness already handles
([ADR 0127](0127-the-opencode-harness-runs-opencode-1.md)): no secrets
declared by the image, one secret per provider captured by the configure
flow, and a delivered file carrying sentinels.

The two differ from opencode, and from each other, in what a harness can
deliver:

- Pi keeps its credentials in one JSON file, `~/.pi/agent/auth.json`, keyed
  by provider, and reads it on every use — opencode's shape exactly.
- omp keeps them in a SQLite database, `~/.omp/agent/agent.db`, beside its
  other state. That is the shape opencode 2 has, and the one ADR 0127 rejected
  building on: a launcher writing another program's schema is a workaround
  against a CLI releasing daily. omp does, however, ship two supported ways in:
  `omp auth-broker import <file> --provider <id>` adds an OAuth credential
  from a JSON file to the local store, and `models.yml` lets a provider's
  `apiKey` name an environment variable to resolve the key from.
- Neither has a command hook. Both reach their lifecycle through an
  extension, as opencode does through a plugin
  ([ADR 0147](0147-opencode-publishes-its-lifecycle-through-an-image-owned-plugin.md)),
  but Pi has no managed configuration layer to name one from, and omp's
  overlay layer (`PI_CONFIG_FILES`) replaces a user's `extensions` list
  rather than adding to it.

## Decision

### 1. Pi is delivered the way opencode is

The configure flow captures `auth.json`, one secret per provider
(`PI_<PROVIDER>_CREDENTIAL`), and returns it as a templated harness file with
sentinels. An OAuth sign-in the control plane can renew (Anthropic, OpenAI
Codex, xAI) is an `oauth` secret; GitHub Copilot's secret is its GitHub
token, delivered in both token fields with no expiry so pi mints an access
token through the proxy; any other is a `token`.

### 2. omp's OAuth sign-ins go through `omp auth-broker import`

The configure flow reads the store with `sqlite3` — one table, the columns
that identify a credential — and returns `.omp/agent/discobox-auth.json`, a
templated file in the importer's own shape. The launcher imports it before
every launch, skipping an entry whose access token is already the active
credential and signing a provider out before importing an entry that differs.
The store is never written by anything but omp.

### 3. omp's API keys go through `models.yml`

The returned `.omp/agent/models.yml` is the user's own file with
`providers.<id>.apiKey: OMP_<PROVIDER>_CREDENTIAL` added for each key, and the
secret's ordinary env delivery exports the sentinel under that name. The file
is written as JSON, which omp reads as YAML; a YAML file a person wrote is
parsed through bun, which omp runs on.

### 4. Both hook extensions are launch flags

`pi --extension` and `omp --hook` load the image-owned extension on every
harness launch. A `pi` or `omp` typed into a shell afterwards publishes
nothing. omp's policy baseline (approvals off, the update check off) is the
image's overlay, which the env names in `PI_CONFIG_FILES`; pi's is the
launch flag `--approve`, since it has no such layer.

## Rejected alternatives

- **Writing omp's `auth_credentials` rows directly.** The columns are
  simple today (`provider`, `credential_type`, JSON `data`), but so was
  opencode 2's, and the reason ADR 0127 gave still holds: a schema is the
  program's, and a launcher that writes it breaks with the next migration
  while reporting success. Reading the store at configure time is a narrower
  bet — one `SELECT`, in a throwaway sandbox, whose failure is a configure
  run that captures nothing — and the only one taken.
- **Environment variables for omp's OAuth tokens.** omp reads
  `ANTHROPIC_OAUTH_TOKEN`, `OPENAI_CODEX_OAUTH_TOKEN`, and a few others, and
  a sentinel could arrive that way. But a Codex sign-in needs the account id
  omp recorded beside the token, some providers have no variable at all, and
  a stray `omp` run under any other name would authenticate with a variable
  it was never configured for. The importer carries the identity and is
  scoped to the store.
- **A `.env` file for omp's API keys.** omp loads `~/.omp/agent/.env`, which
  would deliver a key without touching the user's `models.yml` — but under
  the provider's own variable name, which only omp's bundled catalog maps a
  provider to. `models.yml` names the variable itself, so nothing here has to
  know the catalog.
- **Naming the extension from omp's overlay.** It would make every `omp` in
  the sandbox publish, as the opencode image's managed plugin does. An
  overlay's arrays replace the user's, so it would also silently drop every
  extension the user configured; a launch flag drops nothing.
- **Pi's `settings.json` `extensions` list as a harness file.** The configure
  flow returns the user's settings wholesale, so the entry would have to be
  merged back into whatever they left, and a user removing it would be
  overridden at the next configure. A launch flag is the honest statement
  that the hook is the harness terminal's.

## Consequences

- Two more images ship with every release, on the pool-cached version store
  like the rest (ADR 0114); omp's runs on the bun the sandbox base image
  already installs.
- omp's launch pays the importer's cost — a few `omp` invocations — on the
  first launch of a sandbox and whenever a delivered credential differs from
  the active one; a relaunch with nothing changed pays one `omp token` per
  provider.
- An `omp` typed into a shell, or a pi one, records no hooks. The harness
  terminal does, which is what `discobox wait --hook` and the audit trail
  read.
- The configure flow's reading of `auth_credentials` is version-coupled to
  omp; a schema change there is a configure run that captures nothing and
  says so, never a sandbox that cannot start.
