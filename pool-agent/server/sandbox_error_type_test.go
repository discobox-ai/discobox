package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

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
