//go:build windows

package winlsp

import (
	"testing"
	"unsafe"
)

// SHELLEXECUTEINFOW is 112 bytes on 64-bit Windows; ShellExecuteEx refuses a
// cbSize it does not know.
func TestShellExecuteInfoHasTheSizeWindowsExpects(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("32-bit")
	}
	if size := unsafe.Sizeof(shellExecuteInfo{}); size != 112 {
		t.Fatalf("shellExecuteInfo is %d bytes, want 112", size)
	}
}

// A path with no recorded category reads as an empty one, without elevation.
func TestCategoryOfAPathNothingRecorded(t *testing.T) {
	category, err := Category(`C:\discobox-winlsp-test\nothing-here\discobox-server.exe`)
	if err != nil {
		t.Fatal(err)
	}
	if category&LSPSystem != 0 {
		t.Fatalf("category %#x has LSPSystem", category)
	}
}
