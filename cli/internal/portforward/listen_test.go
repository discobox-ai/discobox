package portforward

import (
	"errors"
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
	// Other processes and OS reservations can occupy later ports too. The
	// search may even exhaust its range and fall back to an ephemeral port.
	if got := listener.Addr().(*net.TCPAddr).Port; got == 0 || got == want || got == want+1 {
		t.Fatalf("bound %d, want a nonzero port other than occupied ports %d and %d", got, want, want+1)
	}
}

// Check the exact search order without assuming any host port is free.
func TestBindNearestSkipsPortsThatAreTaken(t *testing.T) {
	var tried []int
	got, err := bindNearest(8080, DefaultSearch, func(port int) (int, error) {
		tried = append(tried, port)
		if port == 8082 {
			return port, nil
		}
		return 0, errors.New("port unavailable")
	})
	if err != nil {
		t.Fatalf("bindNearest: %v", err)
	}
	if got != 8082 {
		t.Fatalf("bound %d, want 8082", got)
	}
	if want := []int{8080, 8081, 8082}; !reflect.DeepEqual(tried, want) {
		t.Fatalf("tried %v, want %v", tried, want)
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
