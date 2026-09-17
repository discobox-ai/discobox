package poolagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxSharedMemoryIsHalfWhatThePoolCanUse(t *testing.T) {
	host := totalMemoryBytes()
	if host <= 0 {
		t.Skip("no /proc/meminfo to read the host's memory from")
	}
	for _, tc := range []struct {
		name, max string
		want      int64
	}{
		{"unlimited pool", "max", host / 2},
		{"no cgroup file", "", host / 2},
		{"limited pool", "2147483648", 1 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.max != "" {
				if err := os.WriteFile(filepath.Join(root, "memory.max"), []byte(tc.max+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want := tc.want
			if host/2 < want {
				want = host / 2 // a limit above the host's memory is not memory the pool has
			}
			if got := sandboxSharedMemoryBytes(root); got != want {
				t.Fatalf("shm = %d, want %d", got, want)
			}
		})
	}
}
