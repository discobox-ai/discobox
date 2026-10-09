package platform

import (
	"database/sql/driver"
	"fmt"
	"strings"
)

// ImageKind is what a sandbox's image is, and so what a pool must run to start
// it (ADR 26-10-09-106 §4): an OCI image, which a Docker pool runs as a
// container, or a disco-vm image for one driver, which a `discovm` pool on that
// driver runs as a machine. It is a placement key beside the platform, because
// the platform alone does not tell them apart: a boxd pool and an amd64 Docker
// pool both host linux/amd64.
//
// It is spelled `oci` or `discovm/<driver>`. The zero value is no kind: one
// that has not been declared.
type ImageKind struct {
	// Format is FormatOCI or FormatDiscoVM.
	Format string
	// Driver is the disco-vm driver whose image this is — vz, hcs, boxd — and
	// is empty for an OCI image. A disco-vm image is built by and for one
	// driver, so a pool on another runs it no more than a Docker pool does.
	Driver string
}

// The formats an ImageKind names.
const (
	FormatOCI     = "oci"
	FormatDiscoVM = "discovm"
)

// OCI is the kind of every image a Docker pool runs, and of every harness and
// pool from before image kinds were recorded.
var OCI = ImageKind{Format: FormatOCI}

// DiscoVM is the kind of a disco-vm image built for driver.
func DiscoVM(driver string) ImageKind {
	return ImageKind{Format: FormatDiscoVM, Driver: driver}
}

// ParseImageKind reads an `oci` or `discovm/<driver>` kind. The empty string
// is the zero kind.
func ParseImageKind(s string) (ImageKind, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ImageKind{}, nil
	}
	format, driver, _ := strings.Cut(s, "/")
	k := ImageKind{Format: format, Driver: driver}
	if err := k.Validate(); err != nil {
		return ImageKind{}, err
	}
	return k, nil
}

// Validate reports whether k is a usable kind: OCI, or a disco-vm image naming
// its driver. The zero kind is not one.
func (k ImageKind) Validate() error {
	switch {
	case k.Format == FormatOCI && k.Driver == "":
		return nil
	case k.Format == FormatDiscoVM && namePattern.MatchString(k.Driver):
		return nil
	default:
		return fmt.Errorf("image kind %q is neither %s nor %s/<driver>", k.String(), FormatOCI, FormatDiscoVM)
	}
}

// IsZero reports whether no kind has been declared.
func (k ImageKind) IsZero() bool { return k == ImageKind{} }

// String is the `oci` or `discovm/<driver>` spelling, or the empty string for
// the zero kind.
func (k ImageKind) String() string {
	if k.Driver == "" {
		return k.Format
	}
	return k.Format + "/" + k.Driver
}

// MarshalText writes the spelling, so JSON carries a kind as one string.
func (k ImageKind) MarshalText() ([]byte, error) {
	return []byte(k.String()), nil
}

// UnmarshalText reads what MarshalText writes.
func (k *ImageKind) UnmarshalText(text []byte) error {
	parsed, err := ParseImageKind(string(text))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// Value stores a kind as its spelling.
func (k ImageKind) Value() (driver.Value, error) {
	return k.String(), nil
}

// Scan reads a stored spelling.
func (k *ImageKind) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*k = ImageKind{}
		return nil
	case string:
		return k.UnmarshalText([]byte(v))
	case []byte:
		return k.UnmarshalText(v)
	default:
		return fmt.Errorf("cannot scan %T into an image kind", src)
	}
}

// KindMismatchError is a sandbox refused by a pool that runs another kind of
// image. The kinds are the reason, and the message says them both.
type KindMismatchError struct {
	Image ImageKind
	Pool  ImageKind
}

func (e *KindMismatchError) Error() string {
	return fmt.Sprintf("its image is %s, and the pool runs %s", describeImage(e.Image), describePool(e.Pool))
}

// PlaceKind reports whether a sandbox whose image is of kind image may be
// placed on a pool that runs pool: nil when they are the same kind, a
// *KindMismatchError when they are not. A kind that was never declared matches
// nothing, because there is nothing to say it would run.
func PlaceKind(image, pool ImageKind) error {
	if image.IsZero() || image != pool {
		return &KindMismatchError{Image: image, Pool: pool}
	}
	return nil
}

func describeImage(k ImageKind) string {
	switch {
	case k.IsZero():
		return "of no declared kind"
	case k.Format == FormatOCI:
		return "an OCI image"
	default:
		return fmt.Sprintf("a disco-vm image for the %s driver", k.Driver)
	}
}

func describePool(k ImageKind) string {
	switch {
	case k.IsZero():
		return "no declared kind of image"
	case k.Format == FormatOCI:
		return "OCI images"
	default:
		return fmt.Sprintf("disco-vm images for the %s driver", k.Driver)
	}
}
