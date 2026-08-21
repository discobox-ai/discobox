package sandboxcreate

// Step is one stage of creating a sandbox that this client is performing
// itself, in the words a status line says it in.
//
// These are the stages no round trip can report, because this process is the
// one doing them: resolving the source, snapshotting uncommitted work, and
// pushing a source the server cannot reach. Source delivery is exempt from
// every server-side readiness wait for exactly that reason (ADR 0039), so the
// only thing that can narrate it is the client (ADR 0060).
//
// The words live here rather than in each frontend, even though rendering is
// otherwise the frontend's job: `discobox run` and the launcher perform the same
// steps, and two spellings of one stage is a difference users would read as a
// difference in behavior. Where the line is drawn and when it is cleared is
// still each frontend's own.
type Step string

const (
	// StepPreparingSource covers resolving the source ref and snapshotting a
	// dirty workspace. It is one step rather than two because the split is not
	// visible from outside: a clean tree passes through in milliseconds, and a
	// dirty one spends its time in a snapshot the caller already agreed to.
	StepPreparingSource Step = "preparing source"
	StepCreating        Step = "creating the discobox"
	// StepAwaitingSource is the wait for the sandbox to park ready to receive a
	// push. Provisioning has to get that far first, so this is the step that
	// can take a while on a cold pool.
	StepAwaitingSource Step = "waiting for the discobox to accept its source"
	StepPushingSource  Step = "pushing source"
)

// Report receives each step as it begins. A nil Report is valid and reports
// nothing, which is what a non-interactive caller wants.
type Report func(Step)

// step reports one step, tolerating the nil Report so callers never have to.
func (r Report) step(step Step) {
	if r != nil {
		r(step)
	}
}
