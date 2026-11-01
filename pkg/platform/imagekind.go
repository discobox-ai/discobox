package platform

import (
	"database/sql/driver"
	"fmt"
	"strings"
)

// ImageKind is the kind of image a pool runs (ADR 26-10-09-106 §4): OCI
// images, which a Docker pool runs as containers, or VM images for one driver,
// which a `vm` pool on that driver runs as machines. The platform alone does
// not tell them apart: a boxd pool and an amd64 Docker pool both host
// linux/amd64. A sandbox is whatever kind of box its pool runs; a harness names
// no kind (ADR 26-11-01-122).
//
// It is spelled `oci` or `vm/<driver>` (ADR 26-10-15-629). The zero value is
// no kind: one that has not been declared.
type ImageKind struct {
	// Format is FormatOCI or FormatVM.
	Format string
	// Driver is the VM driver whose image this is — vz, hcs, boxd — and is
	// empty for an OCI image. A VM image is built by and for one driver, so a
	// pool on another runs it no more than a Docker pool does.
	Driver string
}

// The formats an ImageKind names.
const (
	FormatOCI = "oci"
	FormatVM  = "vm"
)

// OCI is the kind of every image a Docker pool runs, and of every pool from
// before image kinds were recorded.
var OCI = ImageKind{Format: FormatOCI}

// VM is the kind of a VM image built for driver.
func VM(driver string) ImageKind {
	return ImageKind{Format: FormatVM, Driver: driver}
}

// formatDiscoVM is how releases before ADR 26-10-15-629 spelled FormatVM. It
// is not read; a kind spelled with it is refused with the spelling it now has.
const formatDiscoVM = "discovm"

// ParseImageKind reads an `oci` or `vm/<driver>` kind. The empty string
// is the zero kind. Only those exact spellings are read: one that would be
// written back as another — `oci/` as `oci` — is refused rather than
// normalized, so a stored or declared kind never silently changes.
func ParseImageKind(s string) (ImageKind, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ImageKind{}, nil
	}
	format, driver, _ := strings.Cut(s, "/")
	if format == formatDiscoVM && namePattern.MatchString(driver) {
		return ImageKind{}, fmt.Errorf("image kind %q is spelled %q now", s, VM(driver).String())
	}
	k := ImageKind{Format: format, Driver: driver}
	if err := k.Validate(); err != nil {
		return ImageKind{}, err
	}
	if k.String() != s {
		return ImageKind{}, fmt.Errorf("image kind %q is neither %s nor %s/<driver>", s, FormatOCI, FormatVM)
	}
	return k, nil
}

// Validate reports whether k is a usable kind: OCI, or a VM image naming its
// driver. The zero kind is not one.
func (k ImageKind) Validate() error {
	switch {
	case k.Format == FormatOCI && k.Driver == "":
		return nil
	case k.Format == FormatVM && namePattern.MatchString(k.Driver):
		return nil
	default:
		return fmt.Errorf("image kind %q is neither %s nor %s/<driver>", k.String(), FormatOCI, FormatVM)
	}
}

// IsZero reports whether no kind has been declared.
func (k ImageKind) IsZero() bool { return k == ImageKind{} }

// String is the `oci` or `vm/<driver>` spelling, or the empty string for
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

// NotOCIError is a pool refused a container's tree because it runs another
// kind of image: a tree is a container discobox's, and a machine is not made
// from one (ADR 26-10-09-106 defers it). The pool's kind is the reason.
type NotOCIError struct {
	Pool ImageKind
}

func (e *NotOCIError) Error() string {
	return fmt.Sprintf("the pool runs %s, and a container discobox's tree runs only on a pool of OCI images", describePool(e.Pool))
}

// TakesContainerTree reports whether a pool that runs k takes a container
// discobox's tree: nil for OCI, a *NotOCIError otherwise. A kind that was never
// declared takes nothing, because nothing says the pool would run it.
func (k ImageKind) TakesContainerTree() error {
	if k != OCI {
		return &NotOCIError{Pool: k}
	}
	return nil
}

func describePool(k ImageKind) string {
	switch {
	case k.IsZero():
		return "no declared kind of image"
	case k.Format == FormatOCI:
		return "OCI images"
	default:
		return fmt.Sprintf("VM images for the %s driver", k.Driver)
	}
}
