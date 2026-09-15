package wslc

import (
	"strings"
	"testing"
)

// resize2fs's exit status does not cross the stdio relay, so the guest's
// answer is the only signal the filesystem grew - and when it did not, that
// answer is the reason, which the warning is useless without.
func TestGrowGuestStorageFilesystemReadsTheGuestsAnswer(t *testing.T) {
	t.Run("resized", func(t *testing.T) {
		starter := &fakeStarter{reply: "resized"}
		if err := growGuestStorageFilesystem(t.Context(), starter); err != nil {
			t.Fatalf("growGuestStorageFilesystem: %v", err)
		}
		if !strings.Contains(starter.argv(), `resize2fs "$dev"`) {
			t.Errorf("script does not resize the /var/lib/docker device: %s", starter.argv())
		}
	})

	t.Run("guest refused", func(t *testing.T) {
		starter := &fakeStarter{reply: "resize2fs: Permission denied to resize filesystem"}
		err := growGuestStorageFilesystem(t.Context(), starter)
		if err == nil {
			t.Fatal("growGuestStorageFilesystem reported success for a resize the guest refused")
		}
		if !strings.Contains(err.Error(), "Permission denied") {
			t.Errorf("error = %v, want it to carry what the guest said", err)
		}
		assertStderrFolded(t, starter)
	})
}
