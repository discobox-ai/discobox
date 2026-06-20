# Events Design

`internal/resources/events` owns project event query and subscription service
behavior.

- Read project event rows and resource snapshots through `internal/store`.
- Use `internal/events.Broker` only for live subscription fanout.
- Keep websocket and SSE transport mechanics in `internal/projectstream`.

