package procio

import (
	"strings"
	"testing"
)

// Both platforms' tables are plain data, so both are checked here whatever
// this test runs on.
func TestDeliveriesCoverEverySignalTheProtocolCarries(t *testing.T) {
	for _, table := range []map[string]Delivery{posixDeliveries, windowsDeliveries} {
		for _, name := range []string{"INT", "TERM", "KILL", "HUP", "QUIT", "TSTP", "CONT"} {
			if got := deliveryFor(table, name); got.Delivered == "" {
				t.Errorf("%s is delivered as nothing: %+v", name, got)
			}
		}
	}
}

// On a POSIX platform a signal is delivered as itself, except the one the
// kernel would discard; that one is mapped, and says why.
func TestPOSIXDeliveryMapsOnlyTheSuspend(t *testing.T) {
	for name, want := range map[string]string{"INT": "SIGINT", "sigterm": "SIGTERM", " KILL ": "SIGKILL", "HUP": "SIGHUP", "QUIT": "SIGQUIT", "CONT": "SIGCONT"} {
		got := deliveryFor(posixDeliveries, name)
		if got.Delivered != want || got.Mapped() {
			t.Errorf("deliveryFor(%q) = %+v, want %s as asked", name, got, want)
		}
	}
	suspend := deliveryFor(posixDeliveries, "TSTP")
	if suspend.Requested != "TSTP" || suspend.Delivered != "SIGSTOP" || !strings.Contains(suspend.Reason, "orphaned") {
		t.Fatalf("TSTP = %+v, want SIGSTOP for an orphaned group", suspend)
	}
}

// A Windows process has no signals, so every request is mapped, and none is
// silently turned into a kill: a suspend suspends and a resume resumes.
func TestWindowsDeliveryMapsEverySignalToItsNearestMechanism(t *testing.T) {
	for name, want := range map[string]string{
		"INT": "TerminateProcess", "TERM": "TerminateProcess", "KILL": "TerminateProcess",
		"HUP": "TerminateProcess", "QUIT": "TerminateProcess",
		"TSTP": "NtSuspendProcess", "CONT": "NtResumeProcess",
	} {
		got := deliveryFor(windowsDeliveries, name)
		if got.Delivered != want || !got.Mapped() || !strings.Contains(got.Reason, "Windows process has no") {
			t.Errorf("deliveryFor(%q) = %+v, want %s, mapped, saying why", name, got, want)
		}
	}
}

// A name the protocol does not carry is delivered as nothing, and that too is
// something the record says rather than a request that quietly went nowhere.
func TestUnknownSignalIsDeliveredAsNothingAndSaysSo(t *testing.T) {
	for _, table := range []map[string]Delivery{posixDeliveries, windowsDeliveries} {
		got := deliveryFor(table, "SIGWINCH")
		if got.Requested != "WINCH" || got.Delivered != "" || !got.Mapped() {
			t.Errorf("deliveryFor(SIGWINCH) = %+v, want nothing delivered, mapped", got)
		}
	}
}
