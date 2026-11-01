# discobox-box Review Notes

- **Check each local struct field against `server.yaml`, and have the fake
  serve the API's JSON, not the monitor's structs.** A field read as the
  wrong type fails the whole listing's decode, so every run fails, and a
  fake that encodes the monitor's own struct agrees with it: the first
  draft read a request's `routes` as strings, where the API sends
  `SecretRoute` objects, and only a live request showed it.
- **Show a route with `secretroute.Format`, never a rendering of this
  package's own.** A lead approves a request from what its event says, so a
  route's `graphql` or `mcp` narrowing left out makes a grant read as other
  than it is (a blocked subscription reads as GraphQL refused), and a form
  of this package's own is one no other client shows and `Parse` refuses.
  `route` names any key secretroute does not read.
- **Name a box with `sandboxmeta.Title`.** The server and the console both
  resolve a box's title through it; a rule of this package's own would call
  a box something no other client does.
