// Package clipimage finds the images a paste means on the machine the console
// runs on: the image on the system clipboard, or the image files a pasted path
// names. A discobox has no clipboard of its own and cannot see this machine's
// files, so an agent in one that is told to "paste the image" has nothing to
// paste unless something on this side reads it first. That reading is what is
// here; carrying the bytes into the discobox is the console's.
package clipimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/discobox-ai/discobox/cli/internal/wsl"
)

// Image is one image a paste meant: its bytes, and the name to give them on the
// far side.
type Image struct {
	// Name is a file name with the image's extension: the original file's base
	// name for a file, "clipboard.png" for pixels that never had one.
	Name string
	Data []byte
}

// MaxSize is the largest image read. It is far past any screenshot, and what it
// is there to stop is a paste that names a disk image with a .png on the end
// being read into memory and pushed through an exec stream.
const MaxSize = 32 << 20

// maxPaths is the most paths one paste is taken as. A drag of a whole folder's
// worth of files is not an image paste, it is a file transfer, and `discobox cp`
// is the tool for that.
const maxPaths = 20

// ErrUnavailable is a clipboard there is no way to read here: a Linux with
// neither wl-paste nor xclip, or no display at all — an SSH session.
var ErrUnavailable = errors.New("no clipboard to read on this machine")

// imageExtensions are the formats the agents a discobox runs take as images.
var imageExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// IsImageName reports whether a file name has an image's extension.
func IsImageName(name string) bool {
	return imageExtensions[strings.ToLower(filepath.Ext(name))]
}

// LooksLikePaths reports, without touching the filesystem, whether pasted text
// could be nothing but image paths: every word of it, as a terminal quotes a
// dragged file, ends in an image extension. It is the cheap test that decides
// whether a paste is held back for Paths at all; prose that mentions a.png in
// passing is not, and goes through as typed.
func LooksLikePaths(text string) bool {
	words := splitPaste(text)
	if len(words) == 0 || len(words) > maxPaths {
		return false
	}
	for _, word := range words {
		if !IsImageName(strings.TrimRight(word, "/")) {
			return false
		}
	}
	return true
}

// Paths reads the images pasted text names, when that is all it is: every word
// a path to an image file on this machine. Anything else — a word that is not
// a path, a path to nothing — is not an image paste, and comes back as nil with
// no error, for the paste to go through as text.
//
// The words are what terminals produce when a file is dragged onto them: plain
// absolute paths, paths with their spaces backslash-escaped or the whole thing
// quoted, file:// URIs, and — from Windows Terminal into WSL — Windows paths,
// which wslpath turns into this side's.
func Paths(ctx context.Context, text string) ([]Image, error) {
	if !LooksLikePaths(text) {
		return nil, nil
	}
	var images []Image
	for _, word := range splitPaste(text) {
		path, ok := localPath(ctx, word)
		if !ok {
			return nil, nil
		}
		if !regularFile(path) {
			return nil, nil
		}
		image, err := ReadFile(path)
		if errors.Is(err, ErrNotImage) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, nil
}

// regularFile reports whether path is a file there is to read: a path to
// nothing is not an image paste, and not an error either.
func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// ErrNotImage is a file that is not an image: its name or its bytes say it is
// something else.
var ErrNotImage = errors.New("not an image")

// ReadFile reads one image file, refusing one past MaxSize, and one that is not
// an image by its name and by its bytes.
//
// Both are checked here, on the file actually opened, rather than trusted from
// whatever named it. What names it is often text a discobox wrote — a link an
// agent showed, or put on the clipboard with OSC 52 — and a file:// link's
// query or fragment, or a symlink with .png on the end, can make an image's
// name lead anywhere: what goes into the discobox is only ever an image.
func ReadFile(path string) (Image, error) {
	if !IsImageName(path) {
		return Image{}, fmt.Errorf("%s: %w", filepath.Base(path), ErrNotImage)
	}
	f, err := os.Open(path)
	if err != nil {
		return Image{}, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return Image{}, fmt.Errorf("%s: %w", filepath.Base(path), ErrNotImage)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err != nil {
		return Image{}, err
	}
	if len(data) > MaxSize {
		return Image{}, fmt.Errorf("%s is larger than %d MiB", filepath.Base(path), MaxSize>>20)
	}
	if !imageTypes[http.DetectContentType(data)] {
		return Image{}, fmt.Errorf("%s: %w", filepath.Base(path), ErrNotImage)
	}
	return Image{Name: filepath.Base(path), Data: data}, nil
}

// imageTypes are the content types http.DetectContentType sniffs for the
// formats in imageExtensions.
var imageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// localPath turns one pasted word into a path on this machine, or reports that
// it is not one.
func localPath(ctx context.Context, word string) (string, bool) {
	if strings.HasPrefix(word, "file://") {
		u, err := url.Parse(word)
		if err != nil || (u.Host != "" && u.Host != "localhost") {
			return "", false
		}
		// A dragged file's URI has neither; one that does names something
		// other than what its text ends in.
		if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return "", false
		}
		word = u.Path
		// file:///C:/Users/... is a drive path with a slash in front of it.
		if runtime.GOOS == "windows" && len(word) > 2 && word[0] == '/' && word[2] == ':' {
			word = word[1:]
		}
	}
	if rest, ok := strings.CutPrefix(word, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		word = filepath.Join(home, rest)
	}
	if wsl.Detected() && windowsPath(word) {
		translated, err := wsl.LinuxPath(ctx, word)
		if err != nil {
			return "", false
		}
		word = translated
	}
	// On Windows a path on a share is absolute too, and looking at it connects
	// to that host and signs in as the user. Only a drive path is this
	// machine's. (On WSL, wslpath has already made a share this side's path,
	// and opening that touches no network.)
	if runtime.GOOS == "windows" && len(filepath.VolumeName(word)) != 2 {
		return "", false
	}
	if !filepath.IsAbs(word) {
		return "", false
	}
	return word, true
}

// windowsPath reports whether a word is spelled the way Windows spells a path:
// a drive letter, or a UNC share.
func windowsPath(word string) bool {
	if strings.HasPrefix(word, `\\`) {
		return true
	}
	return len(word) >= 3 && word[1] == ':' && (word[2] == '\\' || word[2] == '/') &&
		unicode.IsLetter(rune(word[0]))
}

// splitPaste splits pasted text into words the way a terminal quoted them.
//
// Whitespace separates words outside quotes. Single quotes are literal, double
// quotes are literal but for an escaped quote, and a backslash outside them
// escapes the character after it only when that character is one a shell
// would need escaping — a space, a quote, a backslash, a parenthesis. Any other
// backslash is kept, which is what leaves C:\Users\me\shot.png alone.
func splitPaste(text string) []string {
	var (
		words   []string
		word    strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	runes := []rune(strings.TrimSpace(text))
	for i, r := range runes {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(runes) && runes[i+1] == '"':
				escaped = true
			default:
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == '\\' && i+1 < len(runes) && strings.ContainsRune(" \t'\"\\()&;[]{}$`!*?#|<>", runes[i+1]):
			escaped = true
			inWord = true
		case unicode.IsSpace(r):
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}
