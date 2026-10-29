package proxyagent

// The registry host: where a sandbox pulls a build's result and publishes its
// local base images, which the pool proxy answers by forwarding to the pool's
// registry front rather than sending to the internet (ADR 26-10-29-160 §1).

// RegistryHost is the host a sandbox names the pool's build registry by. It
// sits beside the gate, origins and assets hosts under the same reserved
// name: all are the pool's, answered by its proxy, and none resolves anywhere
// else.
//
// The front serves it on a Unix socket (layout.ProxyRegistrySocket) for
// AssetsHost's reason: it admits a sandbox to its own namespace by the proxy's
// client header alone.
const RegistryHost = "registry.discobox.internal"
