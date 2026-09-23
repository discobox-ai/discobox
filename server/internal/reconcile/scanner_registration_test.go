package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A reconciler becomes a Scanner by type assertion, so a ScanDirty whose
// signature has drifted would register and never be called: the level-triggered
// backstop stops existing and nothing says so. Registration refuses it instead.
type driftedScanner struct{}

func (driftedScanner) Reconcile(context.Context, string) (Result, error) { return Result{}, nil }

// The signature this once had, which does not satisfy Scanner.
func (driftedScanner) ScanDirty(context.Context, time.Time) ([]string, time.Duration, error) {
	return nil, 0, nil
}

type properScanner struct{}

func (properScanner) Reconcile(context.Context, string) (Result, error) { return Result{}, nil }
func (properScanner) ScanDirty(context.Context) ([]string, error)       { return nil, nil }

type noScanner struct{}

func (noScanner) Reconcile(context.Context, string) (Result, error) { return Result{}, nil }

// A ScanDirty declared on the pointer but registered by value is not in the
// value's method set: the assertion fails and nothing scans, which is the same
// silence by a different route.
type pointerScanner struct{}

func (pointerScanner) Reconcile(context.Context, string) (Result, error) { return Result{}, nil }
func (*pointerScanner) ScanDirty(context.Context, time.Time) ([]string, time.Duration, error) {
	return nil, 0, nil
}

// The shape this repo's own scanners have: everything on the pointer, and the
// pointer registered.
type pointerProperScanner struct{}

func (*pointerProperScanner) Reconcile(context.Context, string) (Result, error) {
	return Result{}, nil
}
func (*pointerProperScanner) ScanDirty(context.Context) ([]string, error) { return nil, nil }

// The likeliest drift of all: somebody forgets the context. The message has to
// name what it found, and this is the case it exists for.
type contextlessScanner struct{}

func (contextlessScanner) Reconcile(context.Context, string) (Result, error) { return Result{}, nil }
func (contextlessScanner) ScanDirty() ([]string, error)                      { return nil, nil }

func TestRegisterNamesTheSignatureItFound(t *testing.T) {
	t.Parallel()
	engine := &Engine{regs: map[string]*registration{}, opt: Options{}}

	err := engine.Register("contextless", contextlessScanner{})
	if err == nil {
		t.Fatal("a ScanDirty with no context registered")
	}
	// Not "(error)", which is what cutting the printed type at its first comma
	// produces: that names a parameter nobody wrote and hides the missing one.
	if !strings.Contains(err.Error(), "ScanDirty() ([]string, error)") {
		t.Fatalf("error = %v, want it to print the signature it found", err)
	}

	err = engine.Register("drifted", driftedScanner{})
	if err == nil || !strings.Contains(err.Error(), "ScanDirty(context.Context, time.Time) ([]string, time.Duration, error)") {
		t.Fatalf("error = %v, want the drifted signature printed as it was written", err)
	}
}

func TestRegisterRefusesAScannerThatWouldNeverRun(t *testing.T) {
	t.Parallel()
	engine := &Engine{regs: map[string]*registration{}, opt: Options{}}

	err := engine.Register("drifted", driftedScanner{})
	if err == nil {
		t.Fatal("a reconciler whose ScanDirty does not satisfy Scanner registered: its scan would never run")
	}
	if !strings.Contains(err.Error(), "ScanDirty") {
		t.Fatalf("error = %v, want it to name the method that drifted", err)
	}

	if err := engine.Register("pointer-drifted", pointerScanner{}); err == nil {
		t.Fatal("a value whose ScanDirty is on the pointer registered: it is not in the value's method set, so nothing would scan")
	}

	// A reconciler that scans properly, and one that does not scan at all, are
	// both fine: scanning is optional, drifting is not.
	if err := engine.Register("proper", properScanner{}); err != nil {
		t.Fatalf("Register(proper) error = %v", err)
	}
	if err := engine.Register("none", noScanner{}); err != nil {
		t.Fatalf("Register(none) error = %v", err)
	}
	if err := engine.Register("pointer-proper", &pointerProperScanner{}); err != nil {
		t.Fatalf("Register(pointer-proper) error = %v", err)
	}
}
