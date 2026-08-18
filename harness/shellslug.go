package harness

// ShellSlug is the reserved slug of the harness that is a plain login shell.
//
// It is reserved rather than special-cased: `shell` is an ordinary registry
// harness built on the sandbox agent base like any other (ADR 0043), and the
// name is held back only so nothing else can claim it (ADR 0032 §3). Callers
// that must tell it apart — the launcher, which will not offer a shell as a
// project's default coding harness — match on this rather than on a literal of
// their own.
const ShellSlug = "shell"
