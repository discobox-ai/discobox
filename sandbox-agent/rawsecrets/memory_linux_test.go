package rawsecrets

import (
	"os"
	"testing"
)

func TestOnMemoryAcceptsTmpfs(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("no /dev/shm")
	}
	if err := onMemory("/dev/shm"); err != nil {
		t.Fatalf("onMemory(/dev/shm) = %v", err)
	}
}

func TestOnMemoryRefusesProc(t *testing.T) {
	if err := onMemory("/proc"); err == nil {
		t.Fatal("onMemory(/proc) accepted a filesystem that is not tmpfs")
	}
}
