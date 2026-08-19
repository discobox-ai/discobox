//go:build windows

package wslcsession

import (
	"errors"
	"strings"
	"testing"
)

// A duplicate session reports itself as such rather than as a bare HRESULT.
//
// 0x800700B7 is the failure a developer actually hits, because a force-killed
// server leaves its VM running and the next start collides with it. Left raw it
// reads like a wslc defect; callers also need to match it to tell "wait for the
// old process to finish exiting" apart from a real failure.
func TestAlreadyExistsIsIdentifiableAndExplained(t *testing.T) {
	// Via a variable: converting the constant directly does not compile, since
	// an HRESULT's high bit makes it exceed int32's range as an untyped value.
	code := hrErrorAlreadyExists
	err := createSessionError(hresultError(int32(code)), "discobox-pool_abc")

	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("error = %v, want it to match ErrSessionExists", err)
	}
	if !strings.Contains(err.Error(), "discobox-pool_abc") {
		t.Fatalf("error = %v, want it to name the session", err)
	}
}

// Every other failure has to pass through untouched, or a real fault is
// reported as a name collision and never diagnosed.
func TestOtherFailuresAreNotReportedAsCollisions(t *testing.T) {
	var eFail uint32 = 0x80004005
	original := hresultError(int32(eFail))
	err := createSessionError(original, "discobox-pool_abc")

	if errors.Is(err, ErrSessionExists) {
		t.Fatalf("error = %v, want an unrelated HRESULT left alone", err)
	}
	if !errors.Is(err, original) {
		t.Fatalf("error = %v, want the original HRESULT preserved", err)
	}
}
