package ui

import "strings"

// Everything in this file is invented. The mockup never talks to a server, so
// the sandboxes below stand in for what `disco ls` would return for the
// current project, most recently used first.

type state string

const (
	stateRunning  state = "running"
	stateStarting state = "starting"
	stateStopped  state = "stopped"
	stateArchived state = "archived"
	stateError    state = "error"
)

// sandbox is the row model: what a picker needs to tell one sandbox from
// another, plus what the actions need to know about whether they apply.
//
// The origin — folder, branch and commit — is what tells two sandboxes with
// similar names apart, so it is on the row rather than behind a key.
type sandbox struct {
	id      string
	name    string
	state   state
	harness string

	folder string // client directory it was started from
	branch string
	commit string // the commit it was spawned from, short
	dirty  bool   // spawned from a snapshot on top of that commit

	// Live usage, from the sandbox agent. Only a sandbox that is up has any.
	//
	// The cpu and the memory are shares of a quota, which is what you would
	// act on. The disk is bytes: the number itself is the thing you want —
	// "31 GiB" tells you what a percentage of a volume you cannot picture
	// does not.
	cpu       int   // percent of its quota
	mem       int   // percent of its quota
	disk      int64 // bytes written to its volume
	diskShare int   // percent of that volume, for the colour

	lastUsed string // since the last attach, exec or apply
	upgrade  bool   // running an image older than its harness config resolves to
	add      int    // lines changed in the sandbox, versus its base commit
	del      int
	files    int
	message  string // error detail, shown when the row is under the cursor
}

func (s sandbox) here() bool    { return s.folder == currentDir }
func (s sandbox) hasDiff() bool { return s.files > 0 }
func (s sandbox) attachable() bool {
	return s.state == stateRunning || s.state == stateStarting || s.state == stateStopped
}

// origin is the folder a sandbox came from, as its last path segment: that is
// the repository's own name, and the leading path is the same for almost every
// row that would ever be on screen together.
func (s sandbox) origin() string {
	if i := strings.LastIndex(s.folder, "/"); i >= 0 {
		return s.folder[i+1:]
	}
	return s.folder
}

// up reports whether the sandbox is running anything, and so whether its
// usage figures mean anything.
func (s sandbox) up() bool {
	return s.state == stateRunning || s.state == stateStarting
}

// base is the commit the sandbox was spawned from. A star marks the ones
// carrying uncommitted work that was snapshotted on top of it.
func (s sandbox) base() string {
	out := s.branch + "@" + s.commit
	if s.dirty {
		out += "*"
	}
	return out
}

const (
	currentDir    = "~/src/disco2"
	currentBranch = "main"

	// The project is a session-wide setting, like `disco -p foo`. The default
	// one is the one you are almost always in, and a header that names it
	// every time teaches you to ignore the header.
	defaultProject = "default"
)

func fakeSandboxes() []sandbox {
	return []sandbox{
		{
			id: "sbx_01j8f3qk", name: "make the pool reaper stop leaking volumes when a sandbox is deleted mid-start", state: stateRunning,
			harness: "claude", folder: currentDir, branch: "main", commit: "a3f9c21", dirty: true,
			cpu: 61, mem: 44, disk: 2_483_027_968, diskShare: 12,
			lastUsed: "2m", add: 142, del: 38, files: 7,
		},
		{
			id: "sbx_01j8dz4w", name: "exec/terminal consolidation", state: stateRunning,
			harness: "claude", folder: currentDir, branch: "main", commit: "a3f9c21",
			cpu: 97, mem: 88, disk: 15_264_268_288, diskShare: 71,
			lastUsed: "18m", add: 903, del: 511, files: 24, upgrade: true,
		},
		{
			id: "sbx_01j8c7ha", name: "openapi: sandbox upgrade field", state: stateStopped,
			harness: "codex", folder: currentDir, branch: "main", commit: "1c713f6",
			lastUsed: "1h", add: 61, del: 12, files: 3,
		},
		{
			id: "sbx_01j8bb2n", name: "userns pool ADR spike", state: stateStarting,
			harness: "claude", folder: currentDir, branch: "main", commit: "1c713f6",
			cpu: 8, mem: 12, disk: 704_643_072, diskShare: 3,
			lastUsed: "1h",
		},
		{
			id: "sbx_01j89xr5", name: "vt screen buffer deadlock", state: stateError,
			harness: "claude", folder: currentDir, branch: "main", commit: "f46b9bb", dirty: true,
			lastUsed: "3h",
			message:  "pool agent lost the sandbox: unit discobox-sbx_01j89xr5.service is gone",
		},
		{
			id: "sbx_01j87m0d", name: "docs: adr 0021 acceptance", state: stateRunning,
			harness: "shell", folder: "~/src/obot", branch: "main", commit: "77e0a44",
			cpu: 2, mem: 31, disk: 11_811_160_064, diskShare: 55,
			lastUsed: "5h", add: 18, del: 4, files: 2, upgrade: true,
		},
		{
			id: "sbx_01j82v9c", name: "bats harness-configure endpoints", state: stateArchived,
			harness: "codex", folder: currentDir, branch: "main", commit: "41a9507",
			lastUsed: "2d", add: 240, del: 96, files: 11,
		},
		{
			id: "sbx_01j7zf6t", name: "cli: recent sandbox resolution", state: stateStopped,
			harness: "cursor", folder: "~/src/disco2-scratch", branch: "wip", commit: "0dc1c14", dirty: true,
			lastUsed: "3d", add: 12, del: 300, files: 5, upgrade: true,
		},
		{
			id: "sbx_01j7w1kp", name: "secret grants split from requests", state: stateArchived,
			harness: "claude", folder: currentDir, branch: "main", commit: "4cd2e2a",
			lastUsed: "6d", add: 512, del: 208, files: 19,
		},
		{
			id: "sbx_01j7q8bn", name: "pool promotion, worker-agent rename", state: stateArchived,
			harness: "codex", folder: currentDir, branch: "main", commit: "9b31f07", dirty: true,
			lastUsed: "11d", add: 1804, del: 1290, files: 63,
		},
	}
}
