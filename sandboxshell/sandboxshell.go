// Package sandboxshell names the one piece of a client's shell preference both
// ends of an interactive shell exec have to agree on: the environment variable
// that carries it.
//
// A person opening a shell in a sandbox — `discobox shell` with no command, or
// a shell pane in the console — gets the shell they use when the sandbox has it, and the sandbox user's login shell when it does not (ADR
// 0138). The preference rides in the exec request's environment rather than in
// a field of its own, and is never stored: it belongs to the person at the
// keyboard, not to the sandbox, so it is read afresh every time a shell opens.
//
// It lives in the root module because the two ends are in different nested
// modules — `cli` sets the variable, `sandbox-agent` acts on it — and a string
// duplicated across two modules is a string that drifts.
package sandboxshell

// PreferredEnv is the exec environment variable naming the shell the client's
// user prefers. The CLI sends its own DISCOBOX_SHELL when set, else `nu` when
// it was run from nushell, else its $SHELL. The value is a path on the client's
// machine or a bare name, so the sandbox treats it as a hint — the path if it
// exists there, otherwise its basename looked up on the sandbox's PATH — and
// never as an argv to run as given.
const PreferredEnv = "DISCOBOX_SHELL"
