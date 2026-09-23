# Reading a CI failure

Shared by `open-pr` and `push-main`. CI is `.github/workflows/ci.yml`; six
jobs: `check` (`ci:check`), `test` (`ci:test`), `verify`, `build`
(`release:build`), `darwin` (`ci:darwin`), `windows` (`ci:test` on Windows).
It takes about seven minutes on Depot's runners; `windows` is the critical
path. Inside a discobox every `gh` call goes through `discobox-access run`.

## Read a finished job while the run is still going

`gh run view --log-failed` refuses while the run is still in progress. To read a
finished job's log while its siblings are still going — which is most of the
time, since `windows` is the longest job — go through the API:

```bash
J=$(gh run view <run-id> --repo discobox-ai/discobox --json jobs \
  -q '.jobs[]|select(.name=="windows")|.databaseId')
gh api repos/discobox-ai/discobox/actions/jobs/$J/logs \
  | sed 's/\x1b\[[0-9;]*m//g' > <scratchpad>/windows.log
grep -nE "(--- FAIL|FAIL\s+github|panic:)" <scratchpad>/windows.log
```

## One failure hides the rest

`test:all` runs the modules in order — root, `cli`, `termpane`, `server`,
`pool-agent`, `sandbox-agent`, `access` — and stops at the first that fails. Every
package *within* a module still runs, but no later module does. A green job
after a fix is not proof the fix was the last problem. Expect to iterate; do
not promise a single round trip.

## What actually breaks on the non-Linux runners

- **Unix socket paths over ~108 bytes.** CI's `TMPDIR` is long and `t.TempDir()`
  adds the test name. Use `shorttmp.Dir(t)` for any directory a socket binds
  under. Reproduce with `TMPDIR=/some/deliberately/long/path go test ./...`.
- **Windows has no POSIX file mode.** `os.Chmod(dir, 0o000)` leaves it readable
  and there is no executable bit. Skip such assertions on
  `runtime.GOOS == "windows"` and say why.
- **`filepath` vs `path`.** Guest paths — anything inside a sandbox or pool, so
  all of `layout` and the sandbox agent's workdirs — are Linux paths on every
  host. Build and compare them with `path`; `filepath.Dir` cleans to backslashes
  on Windows and quietly stops matching.
- **Host paths fed to guest-path code.** The exec manager resolves workdirs as
  guest paths, so a `C:\...` source is read as relative and joined onto the
  working root. Such a test is POSIX-only; skip it on Windows.

Fix these properly rather than skipping wholesale — one of them (a home
directory resolved before checking whether there was anything to install) was a
real launch failure that only Windows exposed.

## Is it this change?

A failure in code the change never reached may already be on `main`: look at
the latest `main` run (`gh run list --repo discobox-ai/discobox --branch main
--workflow CI --limit 5`). Failing there too → pre-existing; say so, do not
fix it inside this change unless the user asks. For a suspected flake, check
the box's own `/proc/loadavg` before trusting a local re-run, and re-run only
the failed jobs (`gh run rerun <run-id> --failed`) once — twice failing is not
a flake.
