# Runner Design

The runner package executes one hook run and returns a complete result. It is the
only package that should spawn user hook processes.

## Responsibilities

- Execute script hooks from the Git root.
- Apply timeouts and cancellation.
- Kill the full process group on Unix when a run is canceled or times out.
- Stream combined stdout/stderr output to the caller-supplied writer while
  retaining an in-memory summary.
- Build the stable `DISCOBOX_` environment contract.
- Return normalized exit codes, duration, output summary, and error metadata.

## Execution Contract

Input should include:

- hook definition
- session ID
- repository root
- run ID
- output writer
- changed files JSON / display string
- inherited or explicit environment values
- timeout

Output should include:

- success boolean
- exit code
- duration
- full output or truncated output summary
- timeout/cancellation flags
- execution error, if process startup failed

## Environment Variables

Runner owns public hook environment variables such as:

- `DISCOBOX_SESSION_ID`
- `DISCOBOX_REPO_ROOT`
- `DISCOBOX_WORKSPACE`
- `DISCOBOX_HOOK_ID`
- `DISCOBOX_HOOK_NAME`
- `DISCOBOX_HOOK_TYPE`
- `DISCOBOX_HOOK_PATH`
- `DISCOBOX_HOOK_PATTERN`
- `DISCOBOX_HOOK_RUN_ID`
- `DISCOBOX_CHANGED_FILES`
- `DISCOBOX_CHANGED_FILES_JSON`
- `DISCOBOX_DB_PATH`
- `DISCOBOX_SOCKET_PATH`

Do not expose secrets unless a future explicit credential policy is added.

## Exit Code Normalization

Use process exit status when available. Reserve normalized codes for local
execution failures:

- startup failure: command could not be started
- timeout: context deadline exceeded
- cancellation: daemon/client canceled the run

Exact numeric values can follow the Discobot executor conventions during
implementation, but must be documented once chosen.

## AI Workflows

Runner does not implement native AI hooks initially. AI behavior is represented
as normal script execution, where the script calls an external AI CLI and exits
`0` for pass or non-zero for feedback/failure.

## Non-Responsibilities

- Do not discover hooks.
- Do not decide which hooks are queued.
- Do not update database state directly.
- Do not own daemon lifecycle or socket APIs.
