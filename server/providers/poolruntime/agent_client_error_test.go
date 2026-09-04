package poolruntime

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	poolclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	poolapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

func poolStatusError(status int, errorType string) error {
	response := poolapimodel.ErrorModel{
		Status: poolclient.NewOptInt64(int64(status)),
		Title:  poolclient.NewOptString(http.StatusText(status)),
	}
	if errorType != "" {
		parsed, err := url.Parse(errorType)
		if err != nil {
			panic(err)
		}
		response.Type = poolclient.NewOptURI(*parsed)
	}
	return &poolclient.ErrorModelStatusCode{StatusCode: status, Response: response}
}

// A conflict is two different conditions on this API, and only the type tells
// them apart. Reporting archived as already-exists let the reconciler swallow a
// create the pool agent had refused, settling a sandbox with no container as
// converged and `ready`.
func TestMapPoolClientErrorSeparatesTheTwoConflicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		errorType string
		want      error
	}{
		{"archived carries its type", http.StatusConflict, poolapimodel.ErrorTypeSandboxArchived, sandbox.ErrArchived},
		{"a bare conflict is already-exists", http.StatusConflict, "", sandbox.ErrAlreadyExists},
		{"an unrelated type is already-exists", http.StatusConflict, "https://discobox.ai/errors/other", sandbox.ErrAlreadyExists},
		{"not found is unchanged", http.StatusNotFound, "", sandbox.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapPoolClientError(poolStatusError(tc.status, tc.errorType)); !errors.Is(got, tc.want) {
				t.Fatalf("mapped = %v, want %v", got, tc.want)
			}
		})
	}
}

// Archived must not read as already-exists anywhere, because that is the one
// error the create path treats as success.
func TestArchivedIsNotAlreadyExists(t *testing.T) {
	if errors.Is(sandbox.ErrArchived, sandbox.ErrAlreadyExists) {
		t.Fatal("ErrArchived matches ErrAlreadyExists; the create path would swallow a refused create")
	}
}
