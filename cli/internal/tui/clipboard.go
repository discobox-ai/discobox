package tui

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"strings"

	"github.com/atotto/clipboard"
)

// osClipboard writes text to the operating system clipboard, or reports that
// it cannot.
//
// WSL gets its own path. The obvious bridges each decode their input by
// whatever code page the Windows side happens to be in — clip.exe reads its
// pipe that way, and Windows Terminal gives OSC 52 payloads the same
// treatment — which is how a "●" pastes as "ΓùÅ". So the text crosses the
// boundary as base64, which is ASCII under every code page there has ever
// been, and PowerShell itself decodes it as the UTF-8 it is.
func osClipboard(ctx context.Context, text string) error {
	if isWSL() {
		b64 := base64.StdEncoding.EncodeToString([]byte(text))
		//nolint:gosec // the interpolated value is base64: its alphabet has no quote or metacharacter, and there is no shell between here and powershell's argv anyway
		return exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-Command",
			"Set-Clipboard -Value ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('"+b64+"')))",
		).Run()
	}
	return clipboard.WriteAll(text)
}

// isWSL reports whether this Linux is a WSL distribution, where the clipboard
// worth writing is Windows' rather than X11's.
func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(release)), "microsoft")
}
