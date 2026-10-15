package platform

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseImageKindRoundTrips(t *testing.T) {
	for s, want := range map[string]ImageKind{
		"oci":     OCI,
		"vm/boxd": VM("boxd"),
		"vm/vz":   VM("vz"),
	} {
		k, err := ParseImageKind(s)
		if err != nil {
			t.Fatalf("ParseImageKind(%q): %v", s, err)
		}
		if k != want || k.String() != s {
			t.Fatalf("ParseImageKind(%q) = %+v (%q), want %+v", s, k, k.String(), want)
		}
	}
}

// A kind as releases before ADR 26-10-15-629 spelled it is refused, with the
// spelling it has now, never read as the VM kind it was.
func TestParseImageKindRefusesTheDiscoVMSpelling(t *testing.T) {
	_, err := ParseImageKind("discovm/boxd")
	if err == nil || !strings.Contains(err.Error(), "vm/boxd") {
		t.Fatalf("ParseImageKind(discovm/boxd) = %v, want a refusal naming vm/boxd", err)
	}
}

func TestParseImageKindEmptyIsZero(t *testing.T) {
	k, err := ParseImageKind(" ")
	if err != nil || !k.IsZero() {
		t.Fatalf("ParseImageKind of blank = %+v, %v; want the zero kind", k, err)
	}
}

func TestParseImageKindRefusesWhatIsNotAKind(t *testing.T) {
	for _, s := range []string{"docker", "oci/", "oci/boxd", "vm", "vm/", "vm/Boxd", "vm/boxd/x", "OCI"} {
		if _, err := ParseImageKind(s); err == nil {
			t.Fatalf("ParseImageKind(%q) accepted it", s)
		}
	}
}

func TestImageKindJSONScanAndValue(t *testing.T) {
	type doc struct {
		Kind ImageKind `json:"imageKind,omitzero"`
	}
	data, err := json.Marshal(doc{Kind: VM("boxd")})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"imageKind":"vm/boxd"}` {
		t.Fatalf("marshaled %s", data)
	}
	if zero, err := json.Marshal(doc{}); err != nil || string(zero) != `{}` {
		t.Fatalf("zero kind marshaled %s, %v", zero, err)
	}
	var back doc
	if err := json.Unmarshal(data, &back); err != nil || back.Kind != VM("boxd") {
		t.Fatalf("unmarshaled %+v, %v", back.Kind, err)
	}
	if err := json.Unmarshal([]byte(`{"imageKind":"vmdk"}`), &back); err == nil {
		t.Fatal("unmarshaled an unknown kind")
	}

	value, err := OCI.Value()
	if err != nil || value != "oci" {
		t.Fatalf("Value() = %v, %v", value, err)
	}
	var scanned ImageKind
	for _, src := range []any{"vm/vz", []byte("vm/vz")} {
		if err := scanned.Scan(src); err != nil || scanned != VM("vz") {
			t.Fatalf("Scan(%v) = %+v, %v", src, scanned, err)
		}
	}
	if err := scanned.Scan(nil); err != nil || !scanned.IsZero() {
		t.Fatalf("Scan(nil) = %+v, %v", scanned, err)
	}
}

func TestPlaceKindRefusesAnotherKind(t *testing.T) {
	if err := PlaceKind(VM("boxd"), VM("boxd")); err != nil {
		t.Fatalf("same kind refused: %v", err)
	}
	if err := PlaceKind(OCI, OCI); err != nil {
		t.Fatalf("OCI on OCI refused: %v", err)
	}
	for _, c := range []struct {
		image, pool ImageKind
		says        string
	}{
		{VM("boxd"), OCI, "its image is a VM image for the boxd driver, and the pool runs OCI images"},
		{OCI, VM("boxd"), "its image is an OCI image, and the pool runs VM images for the boxd driver"},
		{VM("vz"), VM("boxd"), "for the vz driver, and the pool runs VM images for the boxd driver"},
		{ImageKind{}, ImageKind{}, "of no declared kind, and the pool runs no declared kind of image"},
	} {
		err := PlaceKind(c.image, c.pool)
		var mismatch *KindMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("PlaceKind(%s, %s) = %v; want a *KindMismatchError", c.image, c.pool, err)
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Fatalf("PlaceKind(%s, %s) = %q; want it to say %q", c.image, c.pool, err, c.says)
		}
	}
}
