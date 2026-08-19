package endpoint

import (
	"errors"
	"net"
	"net/http"
)

// ErrIrohUnsupported is returned for an iroh endpoint by a binary compiled
// without the iroh build tag.
//
// The scheme is still parsed in this build (see [Parse]) so that the failure
// names the missing capability rather than the scheme. "unsupported scheme"
// sends an operator looking for a typo; this sentence tells them what to do.
var ErrIrohUnsupported = errors.New("this build does not include iroh support; rebuild with -tags iroh")

// The iroh transport is optional, so its entry points are variables holding a
// refusing default that the `iroh` build tag replaces (see iroh_supported.go's
// init).
//
// The alternative — a second file constrained to `!iroh` — is what this
// deliberately avoids. Two mutually exclusive files can never both be analyzed:
// whichever configuration a tool picks, the other file is invisible to it, so
// the language server reports the excluded half as unanalyzable no matter which
// way it is pointed. With the default untagged, `-tags=iroh` compiles every
// file in this package at once and there is no half to exclude.
var (
	configureIroh = func(IrohConfig) error { return ErrIrohUnsupported }

	localIrohID = func() (IrohID, error) { return IrohID{}, ErrIrohUnsupported }

	irohRoundTripper = func(Endpoint, http.RoundTripper) (http.RoundTripper, error) {
		return nil, ErrIrohUnsupported
	}

	irohListen = func(Endpoint) (net.Listener, string, func(), error) {
		return nil, "", nil, ErrIrohUnsupported
	}
)
