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

## Autolaunch probes

- **In `waitReady`, a probe that ran out its own timeout reached a live
  server; only a refused connection or a missing socket or pipe
  (`listenerGone`) means it is gone.** It once counted every probe error
  toward "stopped while starting", and on a host at load 116 three slow
  `/healthz` answers in a row ended the wait of a server busy upgrading its
  pools (#1298); the earlier fix, a short run of failures before giving up,
  was the same mistake.
- **The probes before a launch, and `replaceOlderServer`'s wait for an older
  server to let go, read a timeout as no server on purpose** (`cli/DESIGN.md`,
  autolaunch). Windows refuses a closed loopback TCP port only after about two
  seconds, past the default probe timeout, so reading a timeout there as a
  server stalls every cold start on an `http://` endpoint. Do not "fix" them
  into the `waitReady` rule; telling a dial that timed out from an answer that
  did is #1308. A test that needs to see a refusal on Windows gives its probe
  longer than two seconds.
