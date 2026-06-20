# Sessions Daemon Design

The daemon owns live coding-agent processes. It starts each agent in a PTY,
keeps the process table in memory, exposes local HTTP endpoints over a Unix
socket, and bridges attach streams to PTYs.

The daemon should stay small:

- no server/control-plane imports
- no repository-local runtime files
- no durable database until reconnect-after-daemon-restart is explicitly needed
- one output read loop per PTY
- one framed attach loop per client
