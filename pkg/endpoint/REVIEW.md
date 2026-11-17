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
- **A probe that timed out against a listener is a busy server, not an
  absent one** (`errProbeUnanswered`). A listener is an accepted connection,
  a named pipe whose dial waited (a missing pipe fails at once), or a unix
  connect refused with EAGAIN (a full accept backlog, Linux). Reading it as
  absent launched a second server, which shut the busy one down (#1308).
  `isProbeConnectionError` is false for it, so a caller that does not handle
  it gives up rather than launching.
- **To the probes that decide whether to launch, a TCP dial that timed out
  is not a listener.** Windows refuses a closed loopback port only after about
  two seconds, past the probe timeout; do not widen `errProbeUnanswered` to
  every timeout, or every cold start there stalls. (Inside `waitReady` the
  server is already known to be there, so such a timeout keeps the wait going,
  as above.)
- **Every wait that decides whether to launch ends on the server's answer,
  and that answer is judged for its version** (`probeSettled`). A busy server
  says nothing about its version until it answers; returning once it is ready
  kept an older server the CLI should have replaced. The waits after a launch
  and in `replaceOlderServer` have nothing to judge, and drop it.
- **A wait under the launch lock ends the call** unless the server it found
  is older. Falling through to the launch after waiting out a server that
  appeared started a second one beside it.
