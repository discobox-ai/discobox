# Realtime Design

`internal/realtime` owns project-scoped realtime transports. The canonical
project event transport is the multiplexed websocket route:

```text
GET /projects/{projectId}/stream
```

Clients subscribe by sending JSON control messages over the socket. The initial
supported stream is `sandbox`; future resource streams should be added here with
explicit stream names and tests before being exposed through clients.

Static subscriptions are also exposed through the OpenAPI-documented SSE route:

```text
GET /projects/{projectId}/stream/sse
```

SSE subscriptions are configured entirely by query string. They intentionally do
not emit SSE `id:` fields and do not support `Last-Event-ID`/resume-by-id
semantics. Reconnecting clients should request full history, which is the
default, or explicitly opt out with `history=false`.

## Subscription lifecycle

Each active websocket subscription is keyed by stream-specific subscription
fields and is tracked by a per-subscription token. Cleanup paths must remove or
cancel only the same token they created. Do not delete by key from async cleanup
paths, because a newer subscription may have reused the same key while older
cleanup was still in progress.

Intentional key-only cancellation is limited to explicit replacement or client
unsubscribe behavior, where the current subscription for a key should be
canceled.

## Origin checks

Websocket routes must use the default origin validation from the websocket
library. Do not set insecure origin-check bypass options; browser-accessible
project streams must reject cross-origin websocket requests unless a future
explicit origin policy is added and tested.

## Event source

Both project stream transports reuse the project event service for live events,
history replay, and resource snapshot listing. Transport code should stay
limited to protocol, subscription, filtering, replay/list, and transport
lifecycle concerns.
