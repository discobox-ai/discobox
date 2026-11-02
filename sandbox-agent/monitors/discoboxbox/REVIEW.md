# discobox-box Review Notes

- **Check each local struct field against `server.yaml`, and have the fake
  serve the API's JSON, not the monitor's structs.** A field read as the
  wrong type fails its event's decode, so every run fails, and a
  fake that encodes the monitor's own struct agrees with it: the first
  draft read a request's `routes` as strings, where the API sends
  `SecretRoute` objects, and only a live request showed it.
- **Show a route with `secretroute.Format`, never a rendering of this
  package's own.** A lead approves a request from what its event says, so a
  route's `graphql` or `mcp` narrowing left out makes a grant read as other
  than it is (a blocked subscription reads as GraphQL refused), and a form
  of this package's own is one no other client shows and `Parse` refuses.
  `route` names any key secretroute does not read.
- **Have the fake stream as `internal/server` does**: its replay, the ID-only
  event after it, the cursor and `reset`, with each event's data a map the
  way the store writes it. A framing read wrongly then fails a test rather
  than every run.
- **Save the cursor only where resuming from it loses nothing.** A cursor
  saved part way through a reset's replay skips the rest of it, and the boxes
  it would have confirmed are later taken as deleted. Nothing is concluded —
  a reset confirmed, a named id missing — from a stream that did not run its
  whole hold.
