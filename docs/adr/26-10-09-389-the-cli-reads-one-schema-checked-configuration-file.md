# 26-10-09-389 — The CLI reads one schema-checked configuration file, and a flag still wins

- **Status**: Accepted (supersedes [0096](0096-the-server-reads-one-schema-checked-configuration-file.md)
  §6's deferral of a CLI configuration file)
- **Date**: 2026-10-09
- **Relates to**: [0096](0096-the-server-reads-one-schema-checked-configuration-file.md),
  [26-10-09-395](26-10-09-395-a-sandbox-may-be-created-with-skills.md).

## Context

ADR 0096 §6 gave the server `server.yaml` and deferred one for the CLI "until
something other than this needs it": the CLI took the one setting it shared
with the server, iroh relays, as a flag with an environment fallback.

ADR 26-10-09-395 §4 is that something. `--skills DIR` and `--user-skills` say
which skills a new discobox carries, and a person who wants the same skills in
every discobox has to type them on every `discobox new`. The console's New
Discobox panel cannot take them at all — it offers no way to name a skill
directory — so a discobox created from the console never carries any.

## Decision

### 1. `client.yaml`, beside `server.yaml`, read the way `server.yaml` is

The CLI reads `<xdg.ConfigHome>/discobox/client.yaml`.
`DISCOBOX_CLIENT_CONFIG_FILE` names a different path, and set empty reads none.
It is a separate variable from `DISCOBOX_CONFIG_FILE`, which names the server's
file and which `discobox server --config-file` sets for the server it runs.

Everything ADR 0096 §§1–3 decided carries over: a missing file at the default
path is no file, a named path that does not exist is an error, `Config`'s
struct tags are the source of truth, a generated `cli/config.schema.json` and a
generated commented reference `cli/client.example.yaml` are checked by
`task verify`, and a key nothing defines is an error naming it. A client that
looks for the file and finds none rewrites `client.example.yaml` beside where
it belongs, the way the server rewrites `server.example.yaml`.

The machinery moves out of `server/internal/config` into a root-module
`configfile` package that both read through, so the two files cannot drift in
how they are checked or how their references are written.

### 2. The file holds defaults for flags, and a given flag replaces its setting

The first settings are `new.skills` and `new.userSkills`, the defaults for
`--skills` and `--user-skills`. They apply to `discobox new` — the command
line, `--json`, and the window it opens — and to the console's New Discobox
panel, whose command preview stays the `discobox new` line that would create
the same discobox.

A flag given replaces its setting whole: `--skills DIR` drops `new.skills`
rather than appending to it, and `--user-skills=false` turns `new.userSkills`
off. In `--json`, a field left out takes the setting and `"skills": []` is
none. A relative directory in the file is relative to the file's own directory,
and `~` is the home directory.

`admin box create` does not read the file. It is the flag-driven command that
infers nothing from the local environment, and the file is local environment.

There are no environment variables per setting, unlike the server's. A CLI's
environment is the shell it was typed in, where a flag is already the way to
say something for one command.

## Alternatives rejected

- **Merge a given `--skills` with `new.skills`.** Then nothing on a command
  line could leave a configured directory out, short of pointing
  `DISCOBOX_CLIENT_CONFIG_FILE` elsewhere. Replacement makes a command line
  mean exactly what it says, which is how every other flag here treats the
  default it overrides.
- **Environment variables for these settings.** A second spelling of a
  default with no schema behind it is the problem ADR 0096 was written to
  remove.
- **Copy the server's generator into the CLI module.** The two files would
  then decode, document and render references by two copies of the same
  rules, and the first fix to one would be missing from the other.
- **Keep the CLI configuration in `servers.json` or `<state>`.** `servers.json`
  is the server registry with its own locking and shape, and `<state>` is
  what the CLI keeps for itself rather than what a person writes.

## Consequences

- `<XDG config>/discobox` holds `client.yaml` and `client.example.yaml` beside
  `server.yaml`; `admin uninstall` already removes that directory.
- `cli/config.schema.json` and `cli/client.example.yaml` join the generated
  files `task verify` checks.
- A `client.yaml` that does not parse stops `discobox new` and the console's
  create with the key and file named, before anything is created.
- The next CLI setting is a field on `clientconfig.Config` and nothing else.
