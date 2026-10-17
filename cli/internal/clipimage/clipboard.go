package clipimage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/wsl"
)

// clipboardTimeout bounds one read of the clipboard. PowerShell takes most of
// a second to start, a second time across WSL's interop; a reader that has
// not answered in this long is not going to, and a key is waiting on it.
const clipboardTimeout = 10 * time.Second

// Clipboard reads the images on the system clipboard: the pixels of a
// screenshot or a copied image, or the image files a file manager copied. No
// image is (nil, nil); ErrUnavailable is a machine whose clipboard cannot be
// read at all.
//
// Every platform is asked through the program it already has, since reading an
// image off a clipboard from Go is cgo on two of the three: osascript on
// macOS, wl-paste or xclip on Linux, and PowerShell — across the interop
// boundary, from WSL — on Windows.
func Clipboard(ctx context.Context) ([]Image, error) {
	ctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()
	switch {
	case runtime.GOOS == "darwin":
		return macClipboard(ctx)
	case runtime.GOOS == "windows", wsl.Detected():
		return windowsClipboard(ctx)
	default:
		return unixClipboard(ctx)
	}
}

// macClipboard asks AppleScript what the clipboard holds, and for it.
//
// A file copied in Finder is a «class furl»; anything with pixels — a
// screenshot, an image copied from a browser — converts to «class PNGf» on
// request, TIFF included. The PNG is written by AppleScript to a temporary
// file rather than printed, because what osascript prints of binary data is a
// hex literal.
func macClipboard(ctx context.Context) ([]Image, error) {
	info, err := exec.CommandContext(ctx, "osascript", "-e", "clipboard info").Output()
	if err != nil {
		return nil, fmt.Errorf("read the clipboard: %w", err)
	}
	kinds := string(info)
	if strings.Contains(kinds, "«class furl»") {
		out, err := exec.CommandContext(ctx, "osascript", "-e", "POSIX path of (the clipboard as «class furl»)").Output()
		if err == nil {
			if path := strings.TrimSpace(string(out)); IsImageName(path) {
				image, err := ReadFile(path)
				if err != nil {
					return nil, err
				}
				return []Image{image}, nil
			}
		}
	}
	if !strings.Contains(kinds, "PNGf") && !strings.Contains(kinds, "TIFF") && !strings.Contains(kinds, "JPEG") {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "discobox-clipboard-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "clipboard.png")
	// The path rides as an argument rather than inside the script, so there is
	// no AppleScript quoting to get wrong.
	script := []string{
		"-e", "on run argv",
		"-e", "set png to (the clipboard as «class PNGf»)",
		"-e", "set fh to open for access (POSIX file (item 1 of argv)) with write permission",
		"-e", "write png to fh",
		"-e", "close access fh",
		"-e", "end run",
		path,
	}
	if out, err := exec.CommandContext(ctx, "osascript", script...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("read the clipboard's image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	image, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	return []Image{image}, nil
}

// windowsScript prints the clipboard's image as one "png:<base64>" line, or
// each copied file as a "file:<base64 of its UTF-8 path>" line.
//
// Everything crosses as base64. The image is binary, and a path is text that
// PowerShell would otherwise write in whatever code page the console happens
// to be in — the same reason the text clipboard is written that way.
// Windows.Forms needs a single-threaded apartment, which -STA asks for.
const windowsScript = `Add-Type -AssemblyName System.Windows.Forms, System.Drawing
if ([Windows.Forms.Clipboard]::ContainsImage()) {
	$ms = New-Object IO.MemoryStream
	[Windows.Forms.Clipboard]::GetImage().Save($ms, [Drawing.Imaging.ImageFormat]::Png)
	'png:' + [Convert]::ToBase64String($ms.ToArray())
} elseif ([Windows.Forms.Clipboard]::ContainsFileDropList()) {
	foreach ($f in [Windows.Forms.Clipboard]::GetFileDropList()) {
		'file:' + [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($f))
	}
}`

// windowsClipboard reads Windows' clipboard, from Windows or from WSL.
func windowsClipboard(ctx context.Context) ([]Image, error) {
	powershell := "powershell.exe"
	if wsl.Detected() {
		var err error
		if powershell, err = wsl.PowerShell(ctx); err != nil {
			return nil, fmt.Errorf("read the clipboard: %w", err)
		}
	}
	out, err := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-STA", "-Command", windowsScript).Output()
	if err != nil {
		return nil, fmt.Errorf("read the clipboard: %w", err)
	}
	var images []Image
	lines := bufio.NewScanner(bytes.NewReader(out))
	lines.Buffer(nil, 2*MaxSize)
	for lines.Scan() {
		kind, payload, ok := strings.Cut(strings.TrimSpace(lines.Text()), ":")
		if !ok {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, fmt.Errorf("read the clipboard: %w", err)
		}
		switch kind {
		case "png":
			if len(data) > MaxSize {
				return nil, fmt.Errorf("the clipboard's image is larger than %d MiB", MaxSize>>20)
			}
			images = append(images, Image{Name: "clipboard.png", Data: data})
		case "file":
			path := string(data)
			if !IsImageName(path) {
				continue
			}
			if wsl.Detected() {
				if path, err = wsl.LinuxPath(ctx, path); err != nil {
					return nil, err
				}
			}
			image, err := ReadFile(path)
			if err != nil {
				return nil, err
			}
			images = append(images, image)
		}
	}
	return images, lines.Err()
}

// unixClipboard reads a Wayland or X11 clipboard, whichever this session has
// and has the reader for. The two speak the same MIME types: pixels as an
// image type, files copied in a file manager as a text/uri-list of file://
// URIs.
func unixClipboard(ctx context.Context) ([]Image, error) {
	var list func(context.Context) ([]string, error)
	var read func(context.Context, string) ([]byte, error)
	switch {
	case os.Getenv("WAYLAND_DISPLAY") != "" && onPath("wl-paste"):
		list = func(ctx context.Context) ([]string, error) {
			return outputLines(exec.CommandContext(ctx, "wl-paste", "--list-types"))
		}
		read = func(ctx context.Context, kind string) ([]byte, error) {
			return exec.CommandContext(ctx, "wl-paste", "--no-newline", "--type", kind).Output()
		}
	case os.Getenv("DISPLAY") != "" && onPath("xclip"):
		list = func(ctx context.Context) ([]string, error) {
			return outputLines(exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"))
		}
		read = func(ctx context.Context, kind string) ([]byte, error) {
			return exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", kind, "-o").Output()
		}
	default:
		return nil, ErrUnavailable
	}
	// An empty clipboard is an error to both readers, and not one worth
	// reporting: it lists no types, so there is simply no image on it.
	kinds, _ := list(ctx)
	for _, kind := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		if !slices.Contains(kinds, kind) {
			continue
		}
		data, err := read(ctx, kind)
		if err != nil {
			return nil, fmt.Errorf("read the clipboard's %s: %w", kind, err)
		}
		if len(data) > MaxSize {
			return nil, fmt.Errorf("the clipboard's image is larger than %d MiB", MaxSize>>20)
		}
		return []Image{{Name: "clipboard." + strings.TrimPrefix(kind, "image/"), Data: data}}, nil
	}
	if !slices.Contains(kinds, "text/uri-list") {
		return nil, nil
	}
	uris, err := read(ctx, "text/uri-list")
	if err != nil {
		return nil, fmt.Errorf("read the clipboard's files: %w", err)
	}
	var images []Image
	for _, line := range strings.Split(string(uris), "\n") {
		// A uri-list is CRLF-separated and may carry # comments.
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path, ok := localPath(ctx, line)
		if !ok || !IsImageName(path) {
			continue
		}
		image, err := ReadFile(path)
		if err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, nil
}

func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func outputLines(cmd *exec.Cmd) ([]string, error) {
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
