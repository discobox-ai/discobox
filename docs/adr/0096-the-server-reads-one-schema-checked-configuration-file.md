# 0096 — The server reads one schema-checked configuration file, and the environment still wins

- **Status**: Accepted
- **Date**: 2026-09-07

## Context

`discobox-server` is configured entirely by environment variables. `config.Load`
reads sixteen of them; another six are read by the package that uses them —
`config.ArchiveRetentionEnv`, `imagereap.RetentionEnv`, `devimage.SyncEnv` and
`ManifestEnv`, `dockerworker.PoolImageEnv`, `wslc.WSLCCommandEnv` — so the full
surface is not visible in one place, or in any place. `.discobox-server.env`
exists, but it is environment variables in a file: same namespace, same
absence of a schema.

The sharp end is that a misspelling is not an error. `DISCOBOX_DATA_DIRR` is
not a failure with a message; it is the default, applied silently, and the
server comes up healthy pointing at the wrong directory. Environment variables
cannot be checked for this, because a process's environment is a namespace
shared with everything else — there is nothing to compare against and no way to
tell a typo from a variable meant for something else.

The immediate trigger is iroh relays. n0's public relays are
[free](https://docs.iroh.computer/concepts/relays), which is why discobox works
globally today with nothing configured — but they rate-limit traffic, carry no
uptime or performance guarantee, and are shared across every iroh developer.
A deployment that wants its own has nowhere to put the list:
`endpoint.IrohConfig` has no field for it, and `irohPreset` only ever returns
the presets that select n0's. Adding a twenty-fourth environment variable would
work and would make the invisible surface bigger, at exactly the moment
somebody asked to see it.

## Decision

### 1. One YAML file, whose location is resolved from the environment alone

The server reads `<xdg.ConfigHome>/discobox/server.yaml`.
`DISCOBOX_CONFIG_FILE` names a different path; set to the empty string it reads
nothing. A missing file at the default path is not an error — the environment
alone still configures a server completely, which is what keeps every existing
deployment working.

YAML rather than JSON or TOML because the repository already speaks it —
`api/openapi/server.yaml`, `Taskfile.yml`, the dev loop's `.wnb.yaml` — and
because comments are the point in a file an operator edits by hand.

The file's *location* is deliberately not a setting inside the file.
`configDir` is a setting, so allowing it to relocate the file that declares it
is a loop with no fixed point; naming the path in the environment is the only
place it can come from. This is stated because the natural next request is
"put the config path in the config", and the answer needs to already be
written down.

### 2. The Go struct is the source of truth, and the JSON Schema is generated from it

`config.Config` grows tags — `yaml`, `env`, `default`, `doc` — and a generator
under `server/internal/config/genschema` emits `server/config.schema.json` from
them, in the shape `api/internal/gensandboxopenapi` and `genmodelaliases`
already establish for this repository. `task generate` writes it, and
`task verify` fails when it is stale, exactly like every other generated file.
The published schema is what gives an operator completion and inline
documentation through a `# yaml-language-server: $schema=` modeline.

A hand-written schema was rejected, despite `api/openapi/server.yaml` being
hand-written and canonical. That inversion is right for the API because the
schema there is a contract with clients that outlives any one implementation,
and a human must review a breaking change to it. This schema is a contract with
an operator's editor, derived from what the server can actually load; if the
two disagree, the struct is right and the schema is wrong, which is the
definition of a generated artifact.

An off-the-shelf reflection library was rejected for a smaller reason: it is a
dependency in the module every other module imports, for twenty-odd fields of
scalars, durations and string slices, and it would make its tag vocabulary the
one this repository documents.

### 3. An unknown key fails startup, and says which key

Decoding uses `yaml.Decoder.KnownFields(true)`. A key the schema does not
define is an error naming the key and the file, not a warning and not a
default.

This is the whole reason the file earns its place. Everything else here — the
schema, the completion, the single visible surface — is convenience;
"a misspelling is a failure rather than a silent default" is the capability the
environment cannot provide at any price.

Warning and continuing was rejected: a warning during startup is read by nobody
and is indistinguishable, an hour later, from the server having been configured
correctly.

### 4. Precedence is defaults, then file, then environment

The environment wins. This is what `godotenv` already does with
`.discobox-server.env`, so it is the rule operators of this server have; more
importantly it is what keeps `DISCOBOX_DATA_DIR=/x discobox-server` meaning
what it says, and what lets a container or systemd unit keep configuring the
server exactly as it does today. `.discobox-server.env` continues to work and
is unchanged.

"File wins" was rejected. It reads as the stronger statement that the file is
authoritative, and its actual behavior is to silently ignore a variable an
operator deliberately set on the command line — the failure mode this ADR
exists to remove, reintroduced one layer up.

"File only" was rejected as a breaking change to every existing deployment and
to the dev loop, for a tidiness the precedence rule already delivers.

The loader must distinguish *absent* from *set to the zero value*: `port: 0`
and no `port:` key are different, and a zero field cannot tell them apart. The
decoded YAML node records which keys were present, and that — not the struct's
zero values — is what decides whether an environment variable is overriding
something or filling a gap.

### 5. Every server setting is in the file, including the six read elsewhere

`ArchiveRetentionEnv`, `imagereap.ConfiguredRetention`, `devimage.SyncEnv` and
`ManifestEnv`, `dockerworker.PoolImageEnv` and `wslc.WSLCCommandEnv` become
fields. Those packages take their value from `Config` instead of reaching into
the process environment. A file that claimed to hold all valid configuration
while six settings were read somewhere else would be worse than no file, and
this follows the ownership path rather than leaving a seam.

One exception, and it is not a compromise: `dockerworker/boot.go` sets
`env[imagereap.RetentionEnv]` **on the pool agent container**. That variable is
a wire between two processes, not configuration of this one. The pool agent is
a separate binary with its own environment contract, and it keeps reading it.
What changes is only that the server stops reading that variable for *itself*
(`dockerworker/engine.go`) and takes it from `Config`.

### 6. Custom iroh relays are the file's first new setting, and both ends need configuring

`iroh.relayUrls` reaches `endpoint.IrohConfig.RelayURLs`, and `irohPreset`
selects `iroh.RelayCustom` when it is non-empty. Empty keeps n0's public
relays, which stay the default because they work, cost nothing, and are what
makes a fresh install globally reachable with no setup.

A custom-relay deployment must configure the **client** too, and this is not an
oversight in the setting — it follows from a decision already made.
`parseIrohTicket` deliberately drops the relay a ticket names, on the grounds
that "a deployment that needs a specific relay configures one rather than
inheriting" the peer's opinion. So the server's relays do not travel to the
client, and a client left on n0's relays may fail to reach a server that has
left them. The CLI has no configuration file and is not given one here: it
takes the same setting through the mechanism it already has for everything
else, a flag with an environment fallback. A CLI configuration file is deferred
until something other than this needs it.

Discovery is **not** made configurable, and the reason is not that it was
weighed and declined. `iroh-go` v0.2.0 exposes no knob for it — the Rust FFI's
options carry preset, secret key, ALPNs, relay mode and URLs, and bind
addresses, and nothing else — so a custom `iroh-dns-server` cannot be reached
from here whatever this file says. Upstream iroh does support a custom origin
domain. Revisit when `discobox-ai/iroh-go` exposes it; the field would be
`iroh.discoveryUrl` beside `iroh.relayUrls`, and nothing else about this
decision changes.

## Consequences

- A misspelled setting stops the server with a message naming the key. A
  misspelled environment variable still does not, and cannot; the file is the
  only place this protection exists, which is a reason to prefer it and not a
  reason to remove the environment.
- `server/config.schema.json` joins the generated files `task verify` checks.
  A new setting that does not regenerate it fails CI, which is the mechanism
  that keeps "all valid configuration" true rather than aspirational.
- Four packages stop reading the environment for themselves and take values
  from `Config`. That is a wider change than the file, and it is the part that
  makes the file honest.
- A deployment on its own relays has two things to configure, not one, and a
  half-configured pair fails at connection time rather than at startup. The
  ticket's existing refusal to carry a relay is what makes this so.
- The default remains n0's free public relays and n0's discovery. Nothing about
  a fresh install changes, and a server with no configuration file behaves
  exactly as it does today.
