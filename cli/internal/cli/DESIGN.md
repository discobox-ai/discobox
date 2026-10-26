
## Harness changes

`admin harness changes SANDBOX` previews the immutable candidate the sandbox captured.
Only the server's validated portable settings and credential identities reach
this command. `admin harness save SANDBOX CANDIDATE` commits that exact candidate;
repeating it retries acknowledgment. `admin harness reconsider` requires explicit
`--keep-file` and `--replace-credential` selections before creating a new
candidate against current revisions. `admin harness discard` leaves local files alone.
The console reports a pending candidate from agent status. The configure commands
remain available; save-back does not depend on a configure session.

Harness update requests carry the revision read before editing. The editor keeps
that revision while open and does not reload it at save time; a concurrent
save-back or configuration edit returns a conflict instead of replacing a newer
file bucket. The ordinary update command reads config and egress together once.
