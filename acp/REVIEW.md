# ACP Review Notes

- Keep this module standalone. Do not import server internals or control-plane
  DTOs.
- Keep the supported-agent list explicit; registry presence alone is not enough
  to expose an agent.
- Do not write runtime state into a repository checkout.
- Preserve ACP stdio framing: one UTF-8 JSON-RPC message per line, no
  Content-Length framing.
- Treat registry launch metadata as data. Package-manager launches should be
  resolved directly from registry metadata without Discobox-owned install state.
- Do not advertise filesystem or terminal client capabilities until handlers for
  agent-initiated requests exist.
