package portforward

import (
	"net"
	"reflect"
	"strconv"
	"testing"
)

func TestSearchSpansStartAtThePortItself(t *testing.T) {
	if got := searchSpans(8080, 64); !reflect.DeepEqual(got, []searchSpan{{start: 8080, count: 64}}) {
		t.Fatalf("searchSpans(8080) = %#v", got)
	}
}

// A privileged port gets one try at its own number and then the whole search
// at its unprivileged twin, so 80 lands on 8080 rather than an ephemeral port.
func TestSearchSpansFallBackToTheUnprivilegedTwin(t *testing.T) {
	want := []searchSpan{{start: 80, count: 1}, {start: 8080, count: 64}}
	if got := searchSpans(80, 64); !reflect.DeepEqual(got, want) {
		t.Fatalf("searchSpans(80) = %#v, want %#v", got, want)
	}
	if got := searchSpans(443, 64); got[1].start != 8443 {
		t.Fatalf("searchSpans(443) second span = %#v, want 8443", got[1])
	}
}

func TestListenNearestSkipsPortsThatAreTaken(t *testing.T) {
	first, err := listenTest(t, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer first.Close()
	want := first.Addr().(*net.TCPAddr).Port

	second, err := listenTest(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(want+1)))
	if err != nil {
		t.Skipf("port %d was not available to take: %v", want+1, err)
	}
	defer second.Close()

	listener, err := listenNearest(t.Context(), "127.0.0.1", want, DefaultSearch)
	if err != nil {
		t.Fatalf("listenNearest: %v", err)
	}
	defer listener.Close()
	if got := listener.Addr().(*net.TCPAddr).Port; got != want+2 {
		t.Fatalf("bound %d, want %d — the first free port above %d", got, want+2, want)
	}
}

// A search with nowhere to go still returns a port: any forward beats none,
// and the caller prints the number it got.
func TestListenNearestFallsBackToAnyPort(t *testing.T) {
	taken, err := listenTest(t, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer taken.Close()
	want := taken.Addr().(*net.TCPAddr).Port

	listener, err := listenNearest(t.Context(), "127.0.0.1", want, 0)
	if err != nil {
		t.Fatalf("listenNearest: %v", err)
	}
	defer listener.Close()
	if got := listener.Addr().(*net.TCPAddr).Port; got == 0 || got == want {
		t.Fatalf("bound %d, want some other free port", got)
	}
}
