package proxyagent

// The assets host: where a sandbox fetches the bundles it is pinned to, which
// the pool proxy answers by forwarding to this pool agent's blob cache rather
// than sending to the internet (ADR 26-10-16-423 §13).

// AssetsHost is the host a sandbox fetches its bundles from. It sits beside
// the gate and origins hosts under the same reserved name: all are the
// pool's, answered by its proxy, and none resolves anywhere else.
//
// The pool agent serves it on a Unix socket (layout.ProxyAssetsSocket), not
// a loopback port as the origins are: the listener takes the sandbox from
// the proxy's client header alone, and a loopback port is one a box can send
// its own header to as ordinary egress through the proxy.
const AssetsHost = "assets.discobox.internal"
