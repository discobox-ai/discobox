//go:build windows

package wslcsession

import (
	"errors"
	"strings"
	"testing"
)

// A version comparison that only looked at one component would pick the wrong
// vtable slots, which is not a failed call but a call to a different method:
// Terminate() landing on FormatVirtualDisk, MountWindowsFolder on
// UnmountWindowsFolder. Nothing downstream can detect that, so the ordering
// has to be right here.
func TestVersionOrderingComparesEveryComponent(t *testing.T) {
	for _, tc := range []struct {
		version wslcVersion
		floor   wslcVersion
		want    bool
	}{
		{wslcVersion{2, 9, 10}, wslcVersion{2, 9, 10}, true},
		{wslcVersion{2, 9, 11}, wslcVersion{2, 9, 10}, true},
		{wslcVersion{2, 10, 0}, wslcVersion{2, 9, 10}, true},
		{wslcVersion{3, 0, 0}, wslcVersion{2, 9, 10}, true},
		// 9 is not "less than 10" one digit at a time, which is the mistake
		// a string comparison of "2.9.9" and "2.9.10" would make.
		{wslcVersion{2, 9, 9}, wslcVersion{2, 9, 10}, false},
		{wslcVersion{2, 9, 4}, wslcVersion{2, 9, 5}, false},
		{wslcVersion{2, 8, 12}, wslcVersion{2, 9, 5}, false},
		{wslcVersion{1, 99, 99}, wslcVersion{2, 9, 5}, false},
	} {
		if got := tc.version.atLeast(tc.floor); got != tc.want {
			t.Errorf("wslcVersion%v.atLeast(%v) = %v, want %v", tc.version, tc.floor, got, tc.want)
		}
	}
}

// The slot shift is the whole reason the version is asked for at all: WSL
// 2.9.10 inserted GetEvents as IWSLCSession method 6, and both of the private
// methods still called there sit after it. Both sets are asserted whole,
// because a single wrong number is a silent call to the neighbouring method.
func TestSlotsFollowTheGetEventsInsertion(t *testing.T) {
	before := slotsForVersion(wslcVersion{2, 9, 9})
	want := sessionSlots{
		createRootNamespaceProcess: 23,
		mountWindowsFolder:         26,
	}
	if before != want {
		t.Errorf("slots for 2.9.9 = %+v, want %+v", before, want)
	}

	after := slotsForVersion(wslcVersion{2, 9, 10})
	want = sessionSlots{
		createRootNamespaceProcess: 24,
		mountWindowsFolder:         27,
	}
	if after != want {
		t.Errorf("slots for 2.9.10 = %+v, want %+v", after, want)
	}

	// A newer build keeps the shifted set rather than falling back.
	if got := slotsForVersion(wslcVersion{2, 10, 0}); got != after {
		t.Errorf("slots for 2.10.0 = %+v, want the 2.9.10 set %+v", got, after)
	}
}

// RPC_X_BAD_STUB_DATA is what an ABI change looks like from here, and it is
// the failure this whole version check exists for: a bare "HRESULT
// 0x800706F7" out of CreateSession reads like a broken call rather than a WSL
// update, and cost real time to diagnose the first time.
func TestABIMismatchIsExplainedAndNamesBothVersions(t *testing.T) {
	code := hrRPCXBadStubData
	original := hresultError(int32(code))
	err := abiError(original, wslcVersion{2, 9, 42})

	if !errors.Is(err, original) {
		t.Fatalf("error = %v, want the original HRESULT preserved", err)
	}
	if !strings.Contains(err.Error(), "2.9.42") {
		t.Errorf("error = %v, want it to name the WSL that answered", err)
	}
	if !strings.Contains(err.Error(), derivedVersion.String()) {
		t.Errorf("error = %v, want it to name the WSL these layouts came from", err)
	}
}

// Anything else has to pass through untouched, or an ordinary failure is
// reported as an ABI change and never diagnosed.
func TestOtherFailuresAreNotReportedAsABIMismatches(t *testing.T) {
	var eFail uint32 = 0x80004005
	original := hresultError(int32(eFail))

	err := abiError(original, wslcVersion{2, 9, 10})
	if err != original {
		t.Fatalf("error = %v, want the unrelated HRESULT returned unchanged", err)
	}
	if abiError(nil, wslcVersion{2, 9, 10}) != nil {
		t.Fatal("a successful call must stay successful")
	}
}
