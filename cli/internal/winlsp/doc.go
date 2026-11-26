// Package winlsp reads and sets the Winsock LSP category Windows keeps for an
// executable, and runs a program elevated to set one.
//
// A layered service provider (an LSP, which some VPN and proxy tools install)
// is loaded into every process that uses Winsock, unless the executable's
// category says it permits only LSPs that declare themselves system-safe
// (LSPSystem). The local server unmarshals the sockets WSL hands it for a guest
// process's stdio, and an LSP that cannot take them fails every guest
// operation, so the server is the program a person exempts (ADR 26-11-24-372).
//
// Everything but LSPSystem and ErrDeclined exists on Windows only.
package winlsp

import "errors"

// ErrDeclined is a UAC prompt that was answered no.
var ErrDeclined = errors.New("the administrator prompt was declined")

// LSPSystem is the category bit that admits only LSPs which declare themselves
// safe for system services (LSP_SYSTEM in ws2spi.h). WSL sets it on its own
// executables.
const LSPSystem uint32 = 0x80000000
