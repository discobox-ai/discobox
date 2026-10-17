package provision

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/provisionspec"
)

// Platform is what a step's selector is matched against: the box's GOOS and
// GOARCH, and its os-release ID and ID_LIKE.
type Platform struct {
	OS     string
	Arch   string
	ID     string
	IDLike []string
}

// osReleasePaths are where os-release(5) says to look, in order.
var osReleasePaths = []string{"etc/os-release", "usr/lib/os-release"}

// DetectPlatform is this box's platform, reading os-release under root
// (normally "/").
func DetectPlatform(root string) (Platform, error) {
	platform := Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if platform.OS != "linux" {
		return platform, nil
	}
	for _, rel := range osReleasePaths {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return platform, err
		}
		fields := parseOSRelease(data)
		platform.ID = fields["ID"]
		platform.IDLike = strings.Fields(fields["ID_LIKE"])
		return platform, nil
	}
	return platform, errors.New("no os-release: the box's distribution cannot be told")
}

// parseOSRelease reads os-release's shell-compatible assignments.
func parseOSRelease(data []byte) map[string]string {
	fields := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		}
		fields[key] = value
	}
	return fields
}

// IsDistro reports whether the box is distro by its ID or any ID_LIKE.
func (p Platform) IsDistro(distro string) bool {
	return p.ID == distro || slices.Contains(p.IDLike, distro)
}

// Supported refuses, by name, a box this runner cannot provision. Only Linux
// of the Debian family is supported (ADR 26-10-16-423 §3).
func (p Platform) Supported() error {
	if p.OS != "linux" {
		return fmt.Errorf("provisioning %s is not supported: only Linux of the Debian family is", p.OS)
	}
	if !p.IsDistro("debian") {
		like := ""
		if len(p.IDLike) > 0 {
			like = fmt.Sprintf(" (like %s)", strings.Join(p.IDLike, " "))
		}
		return fmt.Errorf("provisioning distribution %q%s is not supported: only the Debian family is", p.ID, like)
	}
	return nil
}

// Selects reports whether a step selected by s applies to the box. A nil
// selector selects every box.
func (p Platform) Selects(s *provisionspec.Selector) bool {
	if s == nil {
		return true
	}
	if len(s.OS) > 0 && !slices.Contains(s.OS, p.OS) {
		return false
	}
	if len(s.Arch) > 0 && !slices.Contains(s.Arch, p.Arch) {
		return false
	}
	if len(s.Distro) > 0 && !slices.ContainsFunc(s.Distro, p.IsDistro) {
		return false
	}
	return true
}
