# WebSocket recording format

`wsrecord` owns the versioned disk contract between the pool proxy and audit
clients. Application payloads are opaque. `DBW1` identifies version 1; each
record is a big-endian Unix-nanosecond timestamp (8 bytes), direction (1),
opcode (1), payload length (4), and payload. Directions are sent (1) and
received (2); opcodes are RFC 6455 text, binary, close, ping and pong, with 255
reserved for capture diagnostics. EOF at a record boundary ends the available recording; an incomplete record
is a truncated capture, never a partial message. Budget truncation is also
reported by the artifact response header.

Records hold complete, unmasked and decompressed messages, redacted before
writing. Each payload is limited to 16 MiB. Readers enforce the same bound.
`Cut` finds a whole-record boundary for spool-budget truncation without
allocating payloads. The format carries neither HTTP status nor an inferred
request/response relationship.
