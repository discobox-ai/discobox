package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	units "github.com/docker/go-units"
)

// defaultMaxUntracked is the untracked payload a diff will hash without being
// asked twice. It is well above any tree of source files and well below a
// package store or a build directory, which is the distinction that matters:
// the limit exists to catch the case where the answer is "you did not mean to
// diff this", not to ration ordinary work.
const defaultMaxUntracked = "512MiB"

// parseMaxUntracked reads the --max-untracked value, where zero means no limit.
func parseMaxUntracked(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "none" {
		return 0, nil
	}
	limit, err := units.RAMInBytes(value)
	if err != nil {
		return 0, fmt.Errorf("--max-untracked %q is not a size: %w", value, err)
	}
	if limit < 0 {
		return 0, fmt.Errorf("--max-untracked cannot be negative")
	}
	return limit, nil
}

// checkUntrackedPayload refuses a diff whose untracked files would cost more to
// hash than the diff is worth.
//
// Building the right-hand side means `git add`, and `git add` hashes and
// compresses every untracked file into the sandbox repository's object
// database. A package store or a build directory that nothing ignores is
// gigabytes of that, written on every single run — even for `--shortstat`,
// which then prints one line — and left behind as unreachable objects
// afterwards. So the payload is measured before anything is written.
//
// Measuring is cheap relative to what it prevents: `git ls-files -o` is the
// same walk `git add` does, honoring the same ignore rules, without the hashing
// and compression that dominate the cost.
//
// Over the limit the diff stops and names what is in the way, rather than
// quietly leaving it out. A diff that silently omits an agent's work is worse
// than one that refuses and says why, and every remedy — ignore it, narrow the
// pathspec, raise the limit — is the caller's to choose.
func (a *App) checkUntrackedPayload(ctx context.Context, projectID, sandboxID, workdir string, pathspecs []string, limit int64) error {
	if limit <= 0 {
		return nil
	}
	stdout, stderr, code, err := a.sandboxCommandOutput(ctx, projectID, sandboxID, workdir,
		untrackedPayloadCommand(pathspecs, limit))
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("measure the sandbox's untracked files: %s", strings.TrimSpace(stderr+stdout))
	}
	// The command reports only when the limit is exceeded, so anything on stdout
	// is the report and silence is a payload worth hashing.
	if report := strings.TrimSpace(stdout); report != "" {
		return errors.New(report)
	}
	return nil
}

// untrackedPayloadCommand prints why the untracked files are too large to hash,
// and prints nothing at all when they are not.
//
// It runs before the tree is built rather than inside that script so that one
// measurement covers every mode — the streamed diff, the rendered one, and the
// commit `--base local` fetches — and so the report reaches the caller as an
// ordinary error instead of as bytes on a passed-through stderr.
//
// A sandbox whose stat cannot report sizes produces no report and no limit,
// which is the right failure: the guard is an accelerator pedal, not a lock.
func untrackedPayloadCommand(pathspecs []string, limit int64) []string {
	// The awk program is held in shell single quotes, where nothing is
	// interpreted, so it must not contain one itself.
	script := `
list=$(mktemp) || exit 1
trap 'rm -f "$list"' EXIT
git ls-files -o --exclude-standard -z --` + pathspecArgs(pathspecs) + ` > "$list" || exit $?
# xargs with no input runs its command once with no arguments, which stat
# reports as an error; nothing untracked is simply nothing to measure.
[ -s "$list" ] || exit 0
xargs -0 stat -c '%s %n' < "$list" 2>/dev/null | awk -v limit=` + strconv.FormatInt(limit, 10) + ` '
function human(b) {
  if (b >= 1073741824) return sprintf("%.1f GiB", b / 1073741824)
  if (b >= 1048576) return sprintf("%.1f MiB", b / 1048576)
  if (b >= 1024) return sprintf("%.1f KiB", b / 1024)
  return sprintf("%d B", b)
}
{
  # stat puts the size first, so stripping it leaves the path whole however
  # many spaces it contains.
  path = $0
  sub(/^[0-9]+ /, "", path)
  slash = index(path, "/")
  top = slash ? substr(path, 1, slash - 1) : path
  bytes[top] += $1
  files[top] += 1
  total += $1
  count += 1
}
END {
  if (total <= limit) exit
  printf "untracked files come to %s across %d files, over the %s limit\n", human(total), count, human(limit)
  # The largest few, chosen by repeated maximum: there is no portable sort in
  # awk, and three is enough to name the directory that is actually at fault.
  for (i = 0; i < 3; i++) {
    best = ""
    most = -1
    for (name in bytes) {
      if (bytes[name] > most) { most = bytes[name]; best = name }
    }
    if (best == "") break
    printf "  %s: %s across %d files\n", best, human(most), files[best]
    delete bytes[best]
  }
  printf "git would hash all of them into the sandbox to build the diff. Ignore them\n"
  printf "in the sandbox, narrow the diff with a pathspec, or raise --max-untracked\n"
  printf "(--max-untracked 0 removes the limit).\n"
}
'
`
	return []string{"sh", "-c", script}
}
