# Prompter Review Notes

- Keep the public command contract small: optional session ID, prompt, agent,
  model, reasoning, service tier, and current working directory.
- Detection must not perform side effects or depend on secrets.
- Prefer explicit adapter errors over silent fallback to a different agent.
- Keep provider-specific interpretation inside adapters; do not leak provider
  rules into `internal/cli`.
- `internal/agent.CLIRunner` must stay provider-neutral; provider-specific CLI
  flag/session mappings belong in `internal/agent/<agent>/prompt_driver.go` and
  register through the prompt driver registry.
- Prompt mode must emit one normalized JSON result to stdout, not provider-native
  JSON lines.
- Do not add external dependencies until a real adapter requires them.
