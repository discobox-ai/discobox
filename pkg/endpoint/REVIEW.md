# endpoint — review rules

## iroh connections

- **A stream to a peer is opened through `acquire` and given back through
  `release`** (the `irohStream` a round tripper's dial returns does it on
  close). An iroh connection is shared by every caller to its peer, and the
  peer grants it about 100 concurrent streams; `acquire` keeps within
  `irohStreamsPerConn` by dialing another connection only because it counts
  every stream it handed out. A stream opened on a connection directly — as
  `presentCertificate` does on a connection not yet shared — is invisible to
  that count, and enough of them starve every other caller.
- **Bound every dial and every stream open.** An iroh call waits on its
  context alone, and net/http dials on a context nothing cancels: a connect
  with no bound held every request to its peer for good (#1275, #244), and a
  stream open past the peer's limit waits rather than fails.
- **A shared dial is not any one caller's.** It runs on the endpoint's
  context, and each caller waits for it only as long as its own context
  allows; tying it to the caller that happened to start it ends it for
  everyone waiting when that one gives up.
