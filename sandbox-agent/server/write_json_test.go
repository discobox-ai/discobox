package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
)

// A SandboxExec has several optional fields (OptString). ogen's OptString
// marshals to empty bytes via encoding/json when unset, which used to make
// writeJSON 500 with "unexpected end of JSON input" (e.g. on exec start).
// writeJSON must encode ogen types via jx instead.
func TestWriteJSONEncodesOgenTypeWithUnsetOptionals(t *testing.T) {
	rec := httptest.NewRecorder()
	exec := sandboxapi.SandboxExec{ID: "ex_1", Status: "running"} // all OptString fields unset
	writeJSON(rec, http.StatusOK, &exec)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not valid JSON: %v; body=%s", err, rec.Body.String())
	}
	if out["id"] != "ex_1" {
		t.Fatalf("id = %v, want ex_1; body=%s", out["id"], rec.Body.String())
	}
}
