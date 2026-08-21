package sandboxes

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Power operations are instructions, not intent (ADR 0017 §9). Nothing in the
// control plane records that a sandbox should be running: a start is delivered
// to the runtime hosting it, and whatever becomes of it arrives later on the
// runtime's own state-reporting channel.
//
// That is why nothing in this file writes State, DesiredState, or Generation.
// The only thing it persists is sealed secret material the call rotated, which
// is bookkeeping about credentials rather than a claim about the container.

// sandboxInstruction is a power operation forwarded to the runtime.
type sandboxInstruction int

const (
	sandboxStart sandboxInstruction = iota
	sandboxStop
	sandboxRestart
)

func (i sandboxInstruction) String() string {
	switch i {
	case sandboxStop:
		return "stop"
	case sandboxRestart:
		return "restart"
	default:
		return "start"
	}
}

// instructSandbox forwards one power operation to the runtime hosting the
// sandbox.
//
// A sandbox with no resolvable provider is a no-op rather than an error: the
// instruction has nowhere to go, and refusing it would turn a
// provider-less configuration into a failure of the caller's request.
func instructSandbox(ctx context.Context, st *store.Store, provider Provider, sb *model.Sandbox, instruction sandboxInstruction) error {
	if provider == nil {
		return nil
	}
	secretState, err := st.OpenSandboxSecretState(ctx, sb)
	if err != nil {
		return err
	}
	ref := sandboxRefFromSandbox(sb)
	var rotated []byte
	switch instruction {
	case sandboxStop:
		rotated, err = provider.Stop(ctx, ref, secretState, defaultSandboxStopTimeout)
	case sandboxRestart:
		rotated, err = provider.Restart(ctx, ref, secretState, defaultSandboxStopTimeout)
	default:
		rotated, err = provider.Start(ctx, ref, secretState)
	}
	if err != nil {
		return err
	}
	if len(rotated) == 0 && secretState == nil {
		return nil
	}
	return persistSandboxSecretState(ctx, st, sb, rotated)
}

// persistSandboxSecretState writes back only the sealed secret material an
// instruction rotated.
//
// It deliberately re-reads the sandbox and writes without a generation guard.
// An instruction is not intent, so it must not lose a race with one: a delete
// landing concurrently should win the lifecycle fields and still keep the
// credentials the instruction just rotated.
func persistSandboxSecretState(ctx context.Context, st *store.Store, sb *model.Sandbox, secretState []byte) error {
	current, err := st.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		return err
	}
	current.SecretState = secretState
	return st.UpdateSandbox(ctx, current)
}
