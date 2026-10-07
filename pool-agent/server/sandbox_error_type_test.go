package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// Archived and already-exists are both 409, and the control plane has to tell
// them apart to act on either: one means its create is done, the other that
// nothing will run until the sandbox is unarchived. The type is what carries
// that, since the detail is prose and the status is shared.
func TestArchivedErrorCarriesItsType(t *testing.T) {
	service := &sandboxService{}
	response := service.NewError(context.Background(), mapRuntimeError(sandboxruntime.ErrArchived))

	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
	errorType, ok := response.Response.Type.Get()
	if !ok {
		t.Fatal("archived 409 carries no type; the control plane cannot tell it from already-exists")
	}
	if got := errorType.String(); got != workerapimodel.ErrorTypeSandboxArchived {
		t.Fatalf("type = %q, want %q", got, workerapimodel.ErrorTypeSandboxArchived)
	}
	if detail, ok := response.Response.Detail.Get(); !ok || detail != sandboxruntime.ErrArchived.Error() {
		t.Fatalf("detail = %q, want the archived message", detail)
	}
}

// An image the pool cannot obtain is an answer about the sandbox's pin, and the
// control plane records it as the reason the sandbox failed so a client can
// offer the upgrade that fixes it. It is not a 409: an older control plane reads
// every untyped-to-it 409 as "already exists" and would settle the sandbox as
// healthy with no container.
func TestImageUnavailableErrorCarriesItsType(t *testing.T) {
	service := &sandboxService{}
	cause := fmt.Errorf("%w: \"harness:local\" is not on this pool", sandboxruntime.ErrImageUnavailable)
	response := service.NewError(context.Background(), mapRuntimeError(cause))

	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnprocessableEntity)
	}
	errorType, ok := response.Response.Type.Get()
	if !ok || errorType.String() != workerapimodel.ErrorTypeSandboxImageUnavailable {
		t.Fatalf("type = %v, want %q", errorType, workerapimodel.ErrorTypeSandboxImageUnavailable)
	}
	if detail, ok := response.Response.Detail.Get(); !ok || detail != cause.Error() {
		t.Fatalf("detail = %q, want the runtime's message", detail)
	}
}

// Only the errors that name a type get one; everything else stays as it was.
func TestUntypedErrorsCarryNoType(t *testing.T) {
	service := &sandboxService{}
	for name, err := range map[string]error{
		"already exists": mapRuntimeError(sandboxruntime.ErrAlreadyExists),
		"not found":      mapRuntimeError(sandboxruntime.ErrNotFound),
	} {
		t.Run(name, func(t *testing.T) {
			response := service.NewError(context.Background(), err)
			if _, ok := response.Response.Type.Get(); ok {
				t.Fatal("error carries a type it never set")
			}
		})
	}
}

// A sandbox whose tree is here and whose container is not is a third 409, and
// the one the control plane has to answer with "repair it" rather than either
// of the other two.
func TestNoContainerErrorCarriesItsType(t *testing.T) {
	service := &sandboxService{}
	response := service.NewError(context.Background(), mapRuntimeError(sandboxruntime.ErrNoContainer))

	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
	errorType, ok := response.Response.Type.Get()
	if !ok || errorType.String() != workerapimodel.ErrorTypeSandboxNoContainer {
		t.Fatalf("type = %v, want %q", errorType, workerapimodel.ErrorTypeSandboxNoContainer)
	}
}

// refusingRuntime refuses every power instruction with one error.
type refusingRuntime struct {
	sandboxruntime.Runtime
	err error
}

func (r *refusingRuntime) StartSandbox(context.Context, string, *workerapimodel.PoolSandboxOperationRequest) error {
	return r.err
}

func (r *refusingRuntime) StopSandbox(context.Context, string, *workerapimodel.PoolSandboxOperationRequest) error {
	return r.err
}

func (r *refusingRuntime) RestartSandbox(context.Context, string, *workerapimodel.PoolSandboxOperationRequest) error {
	return r.err
}

// A refused power instruction answers with the runtime's own status, not a 500:
// an id this pool does not hold is not found, and a sandbox with no container
// or an archived one is a conflict that says what to do.
func TestPowerInstructionsMapRuntimeErrors(t *testing.T) {
	identity := Identity{ProjectID: "proj_a", PoolID: "pool_a"}
	cases := map[error]struct {
		status    int
		errorType string
	}{
		sandboxruntime.ErrNotFound:    {status: http.StatusNotFound},
		sandboxruntime.ErrNoContainer: {status: http.StatusConflict, errorType: workerapimodel.ErrorTypeSandboxNoContainer},
		sandboxruntime.ErrArchived:    {status: http.StatusConflict, errorType: workerapimodel.ErrorTypeSandboxArchived},
	}
	for cause, want := range cases {
		service := &sandboxService{identity: identity, runtime: &refusingRuntime{err: cause}}
		ctx := context.Background()
		req := &workerapimodel.PoolSandboxOperationRequest{}
		operations := map[string]func() error{
			"start": func() error {
				_, err := service.PoolStartSandbox(ctx, req, workerapi.PoolStartSandboxParams{ProjectId: "proj_a", PoolId: "pool_a", SandboxId: "sbx_1"})
				return err
			},
			"stop": func() error {
				_, err := service.PoolStopSandbox(ctx, req, workerapi.PoolStopSandboxParams{ProjectId: "proj_a", PoolId: "pool_a", SandboxId: "sbx_1"})
				return err
			},
			"restart": func() error {
				_, err := service.PoolRestartSandbox(ctx, req, workerapi.PoolRestartSandboxParams{ProjectId: "proj_a", PoolId: "pool_a", SandboxId: "sbx_1"})
				return err
			},
		}
		for name, operate := range operations {
			t.Run(name+"/"+cause.Error(), func(t *testing.T) {
				response := service.NewError(ctx, operate())
				if response.StatusCode != want.status {
					t.Fatalf("status = %d, want %d", response.StatusCode, want.status)
				}
				errorType, _ := response.Response.Type.Get()
				if got := errorType.String(); got != want.errorType {
					t.Fatalf("type = %q, want %q", got, want.errorType)
				}
			})
		}
	}
}
