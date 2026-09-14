package main

import (
	"testing"

	winversion "github.com/tc-hib/winres/version"
)

func TestParseTag(t *testing.T) {
	for _, tc := range []struct {
		tag        string
		want       [4]uint16
		prerelease bool
	}{
		{tag: "", want: [4]uint16{0, 0, 0, 0}},
		{tag: "v0.7.0", want: [4]uint16{0, 7, 0, 0}},
		{tag: "v1.12.3", want: [4]uint16{1, 12, 3, 0}},
		{tag: "v0.8.0-alpha.2", want: [4]uint16{0, 8, 0, 2}, prerelease: true},
		{tag: "v1.2.3-rc1", want: [4]uint16{1, 2, 3, 1}, prerelease: true},
		{tag: "v1.2.3-beta", want: [4]uint16{1, 2, 3, 0}, prerelease: true},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			got, err := parseTag(tc.tag)
			if err != nil {
				t.Fatal(err)
			}
			if got.numeric != tc.want || got.prerelease != tc.prerelease {
				t.Fatalf("parseTag(%q) = %v prerelease=%v, want %v prerelease=%v",
					tc.tag, got.numeric, got.prerelease, tc.want, tc.prerelease)
			}
		})
	}
}

func TestParseTagRejects(t *testing.T) {
	for _, tag := range []string{
		"1.2.3",        // no v
		"v1.2",         // no patch
		"vm/v6",        // another release line's tag
		"v1.2.3+build", // build metadata
		"v65536.0.0",   // does not fit a field
		"v1.2.3-rc70000",
	} {
		if _, err := parseTag(tag); err == nil {
			t.Errorf("parseTag(%q) succeeded, want an error", tag)
		}
	}
}

// The resource round-trips through its binary form with the fields verify
// checks, so a write and a verify of the same tag agree.
func TestVersionInfoRoundTrip(t *testing.T) {
	info, err := versionInfo("v0.8.0-alpha.2", "discobox.exe", "Discobox CLI")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := winversion.FromBytes(info.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := [4]uint16{0, 8, 0, 2}
	if decoded.FileVersion != want || decoded.ProductVersion != want {
		t.Fatalf("file version %v, product version %v, want %v", decoded.FileVersion, decoded.ProductVersion, want)
	}
	if !decoded.Flags.Prerelease {
		t.Error("prerelease flag not set")
	}
	table := decoded.Table().GetMainTranslation()
	for key, value := range map[string]string{
		winversion.ProductName:      productName,
		winversion.ProductVersion:   "v0.8.0-alpha.2",
		winversion.FileDescription:  "Discobox CLI",
		winversion.OriginalFilename: "discobox.exe",
	} {
		if table[key] != value {
			t.Errorf("%s = %q, want %q", key, table[key], value)
		}
	}
}
