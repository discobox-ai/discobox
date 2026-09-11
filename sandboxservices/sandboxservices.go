// Package sandboxservices names the services Discobox itself declares inside a
// sandbox, so the sandbox that reports one and the client that recognizes it
// agree on the string without either importing the other.
//
// A sandbox's services are declared in files — the repository's under
// `.discobox/services`, the image's under `/usr/local/share/discobox/services`
// (ADR 0094 image services) — and their ids are ordinarily whatever the declaration says.
// Ordinarily nothing outside the sandbox cares: a port is a port, and the
// client forwards it and lists it.
//
// A few are not like that. The desktop is a browser desktop rather than a port
// somebody's program is serving, and a client that knows which one it is can
// put it in its chrome instead of in a list of numbers. That requires a name
// both ends already know, which is what this package is.
//
// It lives in the root module because the two ends are in different nested
// modules — `sandbox-agent` writes the id into its port snapshot, `cli` matches
// on it — and a string duplicated across two modules is a string that drifts.
package sandboxservices

import "strings"

// IDPrefix namespaces the service ids Discobox declares, so a client can
// recognize one without matching on a port number or a filename.
//
// It is reserved for declarations shipped by the image. A repository
// declaration using it is refused, because repository declarations win on a
// shared id: one claiming DesktopID would replace the desktop the image
// shipped and take its affordance with it.
const IDPrefix = "ai.discobox."

// DesktopID is the sandbox's graphical desktop, served over HTTP by the
// socket-activated viewer.
//
// It is the one id a client is expected to special-case. The port behind it is
// not a dev server somebody wants forwarded and listed with the rest — it is a
// desktop, and "open the desktop" is a different offer from "forward port
// 6900".
const DesktopID = IDPrefix + "desktop"

// Reserved reports whether an id is in the namespace only the image may
// declare.
func Reserved(id string) bool { return strings.HasPrefix(id, IDPrefix) }
