package sandbox

// Use is what a request into a sandbox is for (ADR 26-10-12-711 §1). The
// control plane decides by it whether the request starts a stopped sandbox and
// whether it may repair a failed one, and marks the pool token with it so the
// pool agent starts, or refuses to start, by the same answer.
type Use string

const (
	// UseAction changes something in the sandbox or acts in it. It starts a
	// stopped sandbox, and repairs, once, a failed one with no container.
	UseAction Use = "action"
	// UseRead reads what only a running sandbox can answer and has no last
	// value: a transcript, an event stream, resource samples, a file. It
	// starts a stopped sandbox and never repairs one.
	UseRead Use = "read"
	// UseInspect reads what a running sandbox holds that its last inventory
	// also answers: its execs, services and tools (ADR 26-10-11-657 §4). It
	// never starts a sandbox. The pool agent waits out a boot something else
	// began and refuses it for a sandbox that is not running, and the control
	// plane answers that refusal from the inventory.
	UseInspect Use = "inspect"
	// UseObserve reads what a running sandbox is doing, and is refused at once
	// for one that is not running, booting included: starting it would undo
	// the stop being observed, and what polls it wants an answer now.
	UseObserve Use = "observe"
)
