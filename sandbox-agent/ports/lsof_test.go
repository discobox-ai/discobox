package ports

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// lsofFixture is field output in the shape macOS's lsof prints for lsofArgs:
// one p line per process, then per file its f, t, d, P and n fields and, for
// TCP, its T fields.
const lsofFixture = `p512
f5
tIPv4
d0x9b2f6c1e3a4d5f01
PTCP
n127.0.0.1:5173
TST=LISTEN
TQR=0
TQS=0
f6
tIPv6
d0x9b2f6c1e3a4d5f02
PTCP
n*:8080
TST=LISTEN
TQR=0
TQS=0
f7
tIPv4
d0x9b2f6c1e3a4d5f03
PUDP
n*:5353
f8
tIPv4
d0x9b2f6c1e3a4d5f04
PUDP
n127.0.0.1:53000->127.0.0.1:53
f9
tIPv6
d0x9b2f6c1e3a4d5f05
PUDP
n*:*
p513
f5
tIPv4
d0x9b2f6c1e3a4d5f01
PTCP
n127.0.0.1:5173
TST=LISTEN
f6
tIPv6
d0x9b2f6c1e3a4d5f06
PTCP
n[::1]:3000
TST=LISTEN
f7
tIPv6
d0x9b2f6c1e3a4d5f07
PUDP
n*:60001
`

func TestParseLsofReadsListeningAndUnconnectedSockets(t *testing.T) {
	got := parseLsof(lsofFixture)
	type seen struct {
		network network
		addr    string
		port    int
		socket  uint64
	}
	var flat []seen
	for _, entry := range got {
		flat = append(flat, seen{entry.Network, entry.Addr.String(), entry.Port, entry.Socket})
	}
	want := []seen{
		{networkTCP, "127.0.0.1", 5173, 0x9b2f6c1e3a4d5f01},
		{networkTCP, "::", 8080, 0x9b2f6c1e3a4d5f02},
		{networkUDP, "0.0.0.0", 5353, 0x9b2f6c1e3a4d5f03},
		// The connected UDP socket and the one bound to nothing are dropped,
		// and process 513's copy of 5173 is the same socket as 512's.
		{networkTCP, "::1", 3000, 0x9b2f6c1e3a4d5f06},
		{networkUDP, "::", 60001, 0x9b2f6c1e3a4d5f07},
	}
	if !slices.Equal(flat, want) {
		t.Fatalf("parseLsof =\n%+v\nwant\n%+v", flat, want)
	}
}

func TestParseLsofDropsTCPNotListening(t *testing.T) {
	out := "p1\nf3\ntIPv4\nd0x10\nPTCP\nn127.0.0.1:5173\nTST=CLOSED\n"
	if got := parseLsof(out); len(got) != 0 {
		t.Fatalf("parseLsof = %+v, want nothing for a closed socket", got)
	}
}

func TestParseLsofIdentifiesASocketWithNoAddressByDescriptor(t *testing.T) {
	out := "p70\nf4\ntIPv4\nPTCP\nn*:9000\nTST=LISTEN\n"
	got := parseLsof(out)
	if len(got) != 1 || got[0].Socket != 70<<32|4 {
		t.Fatalf("parseLsof = %+v, want one socket identified as pid 70 fd 4", got)
	}
}

func TestParseLsofNameUnmapsV4(t *testing.T) {
	addr, port, ok := parseLsofName("[::ffff:127.0.0.1]:8443", "IPv6")
	if !ok || addr.String() != "127.0.0.1" || port != 8443 {
		t.Fatalf("parseLsofName = %v %d %v, want 127.0.0.1 8443", addr, port, ok)
	}
}

func TestLsofScannerAsksForTheUIDAndDropsEphemeralUDP(t *testing.T) {
	var args []string
	scanner := lsofScanner{
		run: func(_ context.Context, got []string) ([]byte, error) {
			args = got
			return []byte(lsofFixture), nil
		},
		ephemeral: func() portRange { return darwinEphemeralRange },
	}
	got, err := scanner.scan(context.Background(), 501)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !slices.Equal(args, lsofArgs(501)) || !slices.Contains(args, "501") {
		t.Fatalf("lsof args = %v, want lsofArgs(501)", args)
	}
	for _, entry := range got {
		if entry.Network == networkUDP && entry.Port == 60001 {
			t.Fatalf("scan reported UDP 60001, inside the ephemeral range: %+v", got)
		}
	}
	if len(got) != 4 {
		t.Fatalf("scan = %+v, want the four servers", got)
	}
}

func TestLsofScannerReportsAFailedRun(t *testing.T) {
	scanner := lsofScanner{
		run: func(context.Context, []string) ([]byte, error) {
			return nil, errors.New("lsof: exit status 2")
		},
		ephemeral: func() portRange { return darwinEphemeralRange },
	}
	if _, err := scanner.scan(context.Background(), 501); err == nil {
		t.Fatal("scan = nil error, want lsof's failure")
	}
}
