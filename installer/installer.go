// Package installer holds the scripts that install the discobox CLI, and
// stamps the copies a release uploads with what they install (ADR 0109).
//
// It is in the root module for the reason serverstage is: the stamp is a
// release format with two ends. internal/cmd/discobox-installers writes it when
// a release is published, and the scripts read it on a user's machine. Keeping
// the scripts here, embedded, means the release and the tests that run them
// both handle exactly the bytes a user downloads.
package installer

import (
	"bytes"
	_ "embed"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The file names each installer is uploaded as, and served under.
const (
	ShellName      = "install.sh"
	PowerShellName = "install.ps1"
)

//go:embed install.sh
var shellScript []byte

//go:embed install.ps1
var powerShellScript []byte

// Asset is a file a release uploaded that an installer may download.
type Asset struct {
	Name   string
	SHA256 string
}

var (
	releasePattern = regexp.MustCompile(`^v[0-9][0-9A-Za-z.+-]*$`)
	assetPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Stamp returns both installers, keyed by file name, stamped to install
// release from assets.
//
// Every value is checked before it is written into a script: a release, asset
// name, or digest that could close a quote in either language is an error
// here rather than a command on somebody's machine.
func Stamp(release string, assets []Asset) (map[string][]byte, error) {
	if !releasePattern.MatchString(release) {
		return nil, fmt.Errorf("release %q is not a v* version", release)
	}
	if len(assets) == 0 {
		return nil, fmt.Errorf("release %s has no assets to install", release)
	}
	assets = slices.Clone(assets)
	slices.SortFunc(assets, func(a, b Asset) int { return strings.Compare(a.Name, b.Name) })
	for i, asset := range assets {
		if !assetPattern.MatchString(asset.Name) {
			return nil, fmt.Errorf("asset name %q is not a plain file name", asset.Name)
		}
		if !digestPattern.MatchString(asset.SHA256) {
			return nil, fmt.Errorf("asset %s: %q is not a lowercase hex SHA-256", asset.Name, asset.SHA256)
		}
		if i > 0 && assets[i-1].Name == asset.Name {
			return nil, fmt.Errorf("asset %s is listed twice", asset.Name)
		}
	}

	var sums strings.Builder
	sums.WriteString("checksums='\n")
	for _, asset := range assets {
		fmt.Fprintf(&sums, "%s  %s\n", asset.SHA256, asset.Name)
	}
	sums.WriteString("'")
	shell, err := stampLines(ShellName, shellScript, map[string]string{
		"release=":   "release='" + release + "'",
		"checksums=": sums.String(),
	})
	if err != nil {
		return nil, err
	}

	var table strings.Builder
	table.WriteString("$checksums = @{\n")
	for _, asset := range assets {
		fmt.Fprintf(&table, "    '%s' = '%s'\n", asset.Name, asset.SHA256)
	}
	table.WriteString("}")
	powerShell, err := stampLines(PowerShellName, powerShellScript, map[string]string{
		"$release = ''":    "$release = '" + release + "'",
		"$checksums = @{}": table.String(),
	})
	if err != nil {
		return nil, err
	}

	return map[string][]byte{ShellName: shell, PowerShellName: powerShell}, nil
}

// stampLines replaces each line whose content, less its indentation, is a key
// of stamps, keeping that indentation on every line of the replacement. Each
// key has to match exactly one line: a script edited so that a marker moved or
// doubled would otherwise ship unstamped, or stamped twice.
func stampLines(name string, script []byte, stamps map[string]string) ([]byte, error) {
	lines := bytes.SplitAfter(script, []byte("\n"))
	seen := make(map[string]int, len(stamps))
	var out bytes.Buffer
	for _, line := range lines {
		content := strings.TrimRight(string(line), "\r\n")
		trimmed := strings.TrimLeft(content, " \t")
		replacement, ok := stamps[trimmed]
		if !ok {
			out.Write(line)
			continue
		}
		seen[trimmed]++
		indent := content[:len(content)-len(trimmed)]
		for _, stamped := range strings.Split(replacement, "\n") {
			out.WriteString(indent)
			out.WriteString(stamped)
			out.WriteString("\n")
		}
	}
	for marker := range stamps {
		if seen[marker] != 1 {
			return nil, fmt.Errorf("%s: expected one %q line to stamp, found %d", name, marker, seen[marker])
		}
	}
	return out.Bytes(), nil
}
