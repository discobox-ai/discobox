package cli

import (
	"encoding/json"
	"testing"

	apimodel "github.com/discobox-ai/discobox/api/model"
)

// The request as the server sent it, which the console's ctrl+r shows, is
// there for a request whose optional fields are unset: encoded by value, the
// generated types fail on an unset optional and the console had nothing to
// show (issue #1017).
func TestTheConsoleGetsEveryRequestAsSent(t *testing.T) {
	req := toTUICredentialRequest(apimodel.SecretRequest{ID: "sreq_1"})
	if req.Raw == "" || !json.Valid([]byte(req.Raw)) {
		t.Fatalf("credential request raw = %q, want its JSON", req.Raw)
	}
	refresh := toTUIRefreshRequest(apimodel.SecretRefreshRequest{ID: "srr_1", SecretId: "sec_1"})
	if refresh.Raw == "" || !json.Valid([]byte(refresh.Raw)) {
		t.Fatalf("refresh request raw = %q, want its JSON", refresh.Raw)
	}
}
