# Discobox ACP CLI Design

The `discobox-acp` command is the user-facing entrypoint for ACP implementation
launch and inspection.

## Responsibilities

- Show the explicit Discobox-supported ACP implementations.
- Fetch the ACP registry and select supported entries.
- Report launchability and Discobox-launched runtime state.
- Start an ACP supervisor on demand for protocol inspection.
- List ACP sessions through `initialize` and `session/list`.

## Commands

Initial command surface:

- `implementations`: list Discobox-supported ACP implementation IDs.
- `status [agent]`: show current Discobox-recorded runtime state and the resolved
  on-demand launch command.
- `launch [agent]`: start the selected ACP supervisor in the background, wait for
  ACP `initialize` to succeed, then return.
- `sessions list [agent]`: use the running supervisor, or start it on demand,
  then call `session/list`.

Runtime status is based on metadata written by the Discobox ACP supervisor. It
does not attempt to infer agent processes launched by other tools.

## Non-Responsibilities

- Do not implement interactive prompt turns yet.
- Do not expose filesystem or terminal capabilities until the client handles
  those agent-initiated requests.
- Do not manage control-plane server state.
