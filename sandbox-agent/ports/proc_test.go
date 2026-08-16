package ports

import (
	"os"
	"path/filepath"
	"testing"
)

// procNetTCPHeader is the exact header row the kernel writes, kept verbatim so
// the fixtures below are parsed the way a real table is.
const procNetTCPHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestParseProcNetTCPKeepsOnlyListeningSocketsOwnedByUID(t *testing.T) {
	table := procNetTCPHeader +
		// 127.0.0.1:5173, listening, uid 1000 -- wanted.
		"   0: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41001 1 0000 100 0 0 10 0\n" +
		// 0.0.0.0:8080, listening, uid 1000 -- wanted.
		"   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41002 1 0000 100 0 0 10 0\n" +
		// Established, not listening.
		"   2: 0100007F:1436 0100007F:9C40 01 00000000:00000000 00:00000000 00000000  1000        0 41003 1 0000 100 0 0 10 0\n" +
		// Listening but owned by root: a system service, not the sandbox user's.
		"   3: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 41004 1 0000 100 0 0 10 0\n"

	got := parseProcNetTCP(table, 1000)
	if len(got) != 2 {
		t.Fatalf("parseProcNetTCP returned %d listeners, want 2: %+v", len(got), got)
	}
	if got[0].Addr.String() != "127.0.0.1" || got[0].Port != 5173 || got[0].Inode != 41001 {
		t.Errorf("first listener = %+v, want 127.0.0.1:5173 inode 41001", got[0])
	}
	if got[1].Addr.String() != "0.0.0.0" || got[1].Port != 8080 || got[1].Inode != 41002 {
		t.Errorf("second listener = %+v, want 0.0.0.0:8080 inode 41002", got[1])
	}
}

func TestParseProcNetTCPDecodesIPv6AndUnmapsV4(t *testing.T) {
	table := procNetTCPHeader +
		// :: wildcard.
		"   0: 00000000000000000000000000000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 42001 1 0000 100 0 0 10 0\n" +
		// ::1 loopback.
		"   1: 00000000000000000000000001000000:0BB9 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 42002 1 0000 100 0 0 10 0\n" +
		// ::ffff:127.0.0.1 -- an IPv4 socket seen through AF_INET6.
		"   2: 0000000000000000FFFF00000100007F:0BBA 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 42003 1 0000 100 0 0 10 0\n"

	got := parseProcNetTCP(table, 1000)
	if len(got) != 3 {
		t.Fatalf("parseProcNetTCP returned %d listeners, want 3: %+v", len(got), got)
	}
	want := []string{"::", "::1", "127.0.0.1"}
	for i, addr := range want {
		if got[i].Addr.String() != addr {
			t.Errorf("listener %d address = %s, want %s", i, got[i].Addr, addr)
		}
	}
}

func TestParseProcNetTCPSkipsMalformedRows(t *testing.T) {
	table := procNetTCPHeader +
		"   0: garbage\n" +
		"   1: 0100007F:0000 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 43001 1 0000 100 0 0 10 0\n" +
		"   2: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 43002 1 0000 100 0 0 10 0\n"

	got := parseProcNetTCP(table, 1000)
	if len(got) != 1 {
		t.Fatalf("parseProcNetTCP returned %d listeners, want 1 (port 0 and the short row skipped): %+v", len(got), got)
	}
	if got[0].Port != 5173 {
		t.Errorf("listener port = %d, want 5173", got[0].Port)
	}
}

func TestScanListenersReadsBothTablesAndToleratesAMissingOne(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	table := procNetTCPHeader +
		"   0: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 44001 1 0000 100 0 0 10 0\n"
	if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}

	// No net/tcp6: a kernel built without IPv6 has none, and that is not an
	// error -- it reports the sockets it can see.
	got, err := scanListeners(root, 1000)
	if err != nil {
		t.Fatalf("scanListeners error = %v, want nil with net/tcp6 absent", err)
	}
	if len(got) != 1 || got[0].Port != 5173 {
		t.Fatalf("scanListeners = %+v, want the single port 5173 listener", got)
	}
}

func TestScanListenersReportsNothingWithoutProcfs(t *testing.T) {
	got, err := scanListeners(filepath.Join(t.TempDir(), "absent"), 1000)
	if err != nil {
		t.Fatalf("scanListeners error = %v, want nil on a platform with no procfs", err)
	}
	if len(got) != 0 {
		t.Fatalf("scanListeners = %+v, want none", got)
	}
}
