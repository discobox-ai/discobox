package platform

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestParseRoundTrips(t *testing.T) {
	for _, s := range []string{"linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64"} {
		p, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if p.String() != s {
			t.Fatalf("Parse(%q).String() = %q", s, p.String())
		}
	}
}

func TestParseEmptyIsZero(t *testing.T) {
	p, err := Parse("  ")
	if err != nil || !p.IsZero() {
		t.Fatalf("Parse of blank = %+v, %v; want the zero platform", p, err)
	}
}

func TestParseRefusesWhatIsNotAPair(t *testing.T) {
	for _, s := range []string{"linux", "linux/", "/amd64", "linux/amd64/v8", "Linux/amd64", "linux/../x"} {
		if _, err := Parse(s); err == nil {
			t.Fatalf("Parse(%q) accepted it", s)
		}
	}
}

func TestPoolIsLinuxOnThisArchitecture(t *testing.T) {
	if got := Pool(); got.OS != "linux" || got.Arch != runtime.GOARCH {
		t.Fatalf("Pool() = %s", got)
	}
}

func TestJSONIsOneString(t *testing.T) {
	type doc struct {
		Platform Platform `json:"platform"`
	}
	data, err := json.Marshal(doc{Platform: Platform{OS: "darwin", Arch: "arm64"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"platform":"darwin/arm64"}` {
		t.Fatalf("marshaled %s", data)
	}
	var back doc
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Platform != (Platform{OS: "darwin", Arch: "arm64"}) {
		t.Fatalf("unmarshaled %+v", back.Platform)
	}
	if err := json.Unmarshal([]byte(`{"platform":"darwin"}`), &back); err == nil {
		t.Fatal("unmarshaled a platform with no architecture")
	}
}

func TestScanAndValue(t *testing.T) {
	var p Platform
	for _, src := range []any{"linux/arm64", []byte("linux/arm64")} {
		if err := p.Scan(src); err != nil || p != (Platform{OS: "linux", Arch: "arm64"}) {
			t.Fatalf("Scan(%v) = %+v, %v", src, p, err)
		}
	}
	if err := p.Scan(nil); err != nil || !p.IsZero() {
		t.Fatalf("Scan(nil) = %+v, %v", p, err)
	}
	value, err := Platform{OS: "linux", Arch: "amd64"}.Value()
	if err != nil || value != "linux/amd64" {
		t.Fatalf("Value() = %v, %v", value, err)
	}
}

func TestPlaceRefusesAnotherPlatform(t *testing.T) {
	linux := Platform{OS: "linux", Arch: "arm64"}
	if err := Place(linux, linux); err != nil {
		t.Fatalf("Place on its own platform: %v", err)
	}
	for _, pool := range []Platform{{OS: "linux", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"}, {}} {
		err := Place(linux, pool)
		var mismatch *MismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("Place(%s, %s) = %v; want a mismatch", linux, pool, err)
		}
		if !strings.Contains(err.Error(), "linux/arm64") {
			t.Fatalf("the refusal does not name the sandbox's platform: %v", err)
		}
	}
	if err := Place(Platform{}, Platform{}); err == nil {
		t.Fatal("an undeclared sandbox platform was placed")
	}
}
