// Command discobox-winres gives a Windows release binary the version resource
// Windows reads, and checks that a built one carries it.
//
// A Go binary has no VS_VERSIONINFO resource, and the -X linker flag that
// stamps version.Version is invisible to Windows: Get-Command, Explorer's
// Details tab, and anything else that asks the file rather than the program
// see 0.0.0.0. The resource has to be linked in, which Go does for any
// *_windows_<arch>.syso object in the main package's directory.
//
//	discobox-winres write -version v1.2.3 -arch amd64 \
//	  -name discobox.exe -description "Discobox CLI" \
//	  -out cli/cmd/discobox/zz_version_windows_amd64.syso
//
//	discobox-winres verify -version v1.2.3 build/release/bin/discobox-windows-amd64.exe
//
// verify exists because a .syso that is not in the package directory, or not
// named for the target architecture, is ignored without a word: the build
// succeeds and the binary is back to 0.0.0.0.
//
// An empty -version is an untagged build. It still gets the resource, so the
// build that proves every push can still produce a release proves this too, and
// reports 0.0.0.0 because there is no release for it to name.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/tc-hib/winres"
	winversion "github.com/tc-hib/winres/version"
)

const productName = "Discobox"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-winres:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: discobox-winres write|verify [flags]")
	}
	switch args[0] {
	case "write":
		return runWrite(args[1:])
	case "verify":
		return runVerify(args[1:])
	default:
		return fmt.Errorf("unknown command %q: want write or verify", args[0])
	}
}

func runWrite(args []string) error {
	flags := flag.NewFlagSet("write", flag.ContinueOnError)
	var (
		tag         = flags.String("version", "", "release tag, vMAJOR.MINOR.PATCH[-PRERELEASE]; empty for an untagged build")
		arch        = flags.String("arch", "", "GOARCH the object is for")
		name        = flags.String("name", "", "file name the executable is run as")
		description = flags.String("description", "", "what the executable is, as Explorer shows it")
		out         = flags.String("out", "", "path of the .syso to write")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *arch == "" || *name == "" || *description == "" || *out == "" {
		return errors.New("write: -arch, -name, -description, and -out are required")
	}
	// Go links a .syso only when its name says which architecture it is for, so
	// one written under any other name builds cleanly and embeds nothing.
	if want := "_windows_" + *arch + ".syso"; !strings.HasSuffix(filepath.Base(*out), want) {
		return fmt.Errorf("write: -out %s must end in %s or go build will not link it", *out, want)
	}

	info, err := versionInfo(*tag, *name, *description)
	if err != nil {
		return err
	}
	var resources winres.ResourceSet
	resources.SetVersionInfo(info)

	var object bytes.Buffer
	if err := resources.WriteObject(&object, winres.Arch(*arch)); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	return os.WriteFile(*out, object.Bytes(), 0o600)
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	tag := flags.String("version", "", "release tag the binaries should carry; empty for an untagged build")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("verify: no executables named")
	}
	want, err := parseTag(*tag)
	if err != nil {
		return err
	}
	for _, path := range flags.Args() {
		got, err := readVersionInfo(path)
		if err != nil {
			return err
		}
		if got.FileVersion != want.numeric || got.ProductVersion != want.numeric {
			return fmt.Errorf("%s: carries file version %s and product version %s, not %s",
				path, dotted(got.FileVersion), dotted(got.ProductVersion), dotted(want.numeric))
		}
		table := got.Table().GetMainTranslation()
		if table[winversion.ProductName] != productName {
			return fmt.Errorf("%s: product name is %q, not %q", path, table[winversion.ProductName], productName)
		}
		if *tag != "" && table[winversion.ProductVersion] != *tag {
			return fmt.Errorf("%s: product version string is %q, not %q", path, table[winversion.ProductVersion], *tag)
		}
		fmt.Printf("%s: Windows version %s\n", filepath.Base(path), dotted(got.FileVersion))
	}
	return nil
}

func readVersionInfo(path string) (*winversion.Info, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	resources, err := winres.LoadFromEXESingleType(file, winres.RT_VERSION)
	if err != nil {
		return nil, fmt.Errorf("%s: no version resource, so the .syso did not reach the link: %w", path, err)
	}
	var data []byte
	resources.WalkType(winres.RT_VERSION, func(_ winres.Identifier, _ uint16, resource []byte) bool {
		data = resource
		return false
	})
	if data == nil {
		return nil, fmt.Errorf("%s: no version resource: the .syso did not reach the link", path)
	}
	info, err := winversion.FromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("%s: unreadable version resource: %w", path, err)
	}
	return info, nil
}

// versionInfo is the resource for one executable of the release tag names.
func versionInfo(tag, name, description string) (winversion.Info, error) {
	release, err := parseTag(tag)
	if err != nil {
		return winversion.Info{}, err
	}
	info := winversion.Info{
		FileVersion:    release.numeric,
		ProductVersion: release.numeric,
	}
	info.Flags.Prerelease = release.prerelease

	display := tag
	if display == "" {
		display = dotted(release.numeric)
	}
	for key, value := range map[string]string{
		winversion.ProductName:      productName,
		winversion.ProductVersion:   display,
		winversion.FileVersion:      display,
		winversion.FileDescription:  description,
		winversion.OriginalFilename: name,
		winversion.InternalName:     name,
	} {
		if err := info.Set(winversion.LangDefault, key, value); err != nil {
			return winversion.Info{}, err
		}
	}
	return info, nil
}

type release struct {
	numeric    [4]uint16
	prerelease bool
}

var tagPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

var trailingNumber = regexp.MustCompile(`(\d+)$`)

// parseTag turns a release tag into the four 16-bit fields Windows compares
// and shows.
//
// MAJOR.MINOR.PATCH are the first three. The fourth is the number a prerelease
// ends in (v1.2.3-alpha.2 and v1.2.3-rc2 are both 1.2.3.2), and 0 for a dot
// release, so the builds of one release can be told apart. That puts a
// prerelease numerically after the release it precedes, which nothing reads the
// field to decide — winget and Homebrew order by the tag — and the full tag is
// in the resource's version strings.
func parseTag(tag string) (release, error) {
	if tag == "" {
		return release{}, nil
	}
	match := tagPattern.FindStringSubmatch(tag)
	if match == nil {
		return release{}, fmt.Errorf("version %q is not a vMAJOR.MINOR.PATCH[-PRERELEASE] tag", tag)
	}
	var parsed release
	for i, field := range match[1:4] {
		value, err := field16(tag, field)
		if err != nil {
			return release{}, err
		}
		parsed.numeric[i] = value
	}
	if prerelease := match[4]; prerelease != "" {
		parsed.prerelease = true
		if number := trailingNumber.FindString(prerelease); number != "" {
			value, err := field16(tag, number)
			if err != nil {
				return release{}, err
			}
			parsed.numeric[3] = value
		}
	}
	return parsed, nil
}

func field16(tag, field string) (uint16, error) {
	value, err := strconv.ParseUint(field, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("version %q: %s does not fit a Windows version field (0-65535)", tag, field)
	}
	return uint16(value), nil
}

func dotted(v [4]uint16) string {
	return fmt.Sprintf("%d.%d.%d.%d", v[0], v[1], v[2], v[3])
}
