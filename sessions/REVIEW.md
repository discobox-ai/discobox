# Sessions Review Notes

- Keep this module standalone. Do not import server internals or control-plane
  DTOs.
- Preserve session scoping: one daemon/socket namespace per Discobox session and
  Git repository.
- Keep daemon-owned PTY processes authoritative. CLI commands must not start
  agents directly except for the daemon foreground command.
- Do not write runtime state into a repository checkout.
- Keep supported agents explicit; config may override commands but must not make
  arbitrary unknown agents silently supported.
- Preserve detach semantics: `ctrl+p q` closes the attach stream without killing
  the agent process.
- Keep terminal resize and signal delivery routed through the daemon so multiple
  clients can attach consistently.
