<div align="center">
  <img src="assets/brand/wordmark-gradient.svg" alt="Discobox" width="460">
</div>

Discobox runs coding agents in disposable environments with a copy of your
source. Agents have passwordless sudo, nested Docker, a desktop, and a browser.
You can connect through a terminal, SSH, or your editor, and bring changes back
as git commits.

Claude Code and Codex are included; other terminal agents can be packaged in an
image. Discobox supports macOS, Linux, and Windows and is under active
development.

## Getting started

Install with Homebrew:

```bash
brew install discobox-ai/tap/discobox
```

Open the launcher from your repository to create a box:

```bash
cd ~/src/my-project
discobox
```

Work with the agent and have it commit the changes inside the box. From your
original repository, apply those commits to your working tree:

```bash
discobox apply
```

Each box has its own git remote. Applying cherry-picks its commits, so you can
review the resulting history with your usual git tools. Multiple boxes can work
on the same source independently.

![Discobox with an agent terminal, a shell, services, and forwarded ports](assets/screens/claude-code.png)

## Working with a box

- **Terminal and SSH:** Use the TUI or `discobox shell`. SSH configuration syncs
  automatically when a box is created, so `ssh $DISCOBOX_ID` works without manual
  setup. You can also connect by box name.
- **Editor:** Use `discobox tools vscode` to open VS Code in the box's working
  directory. Editors with SSH remote support can also connect directly.
- **Desktop:** Access the box's graphical desktop and browser through VNC or
  noVNC, using port forwarding.
- **Toolchain:** direnv loads the project's declared environment, including
  Nix or mise configuration.
- **Automation:** The CLI and OpenAPI API support scripting box creation,
  credential grants, and collecting results.

## Isolation and credentials

Discobox uses VM and container isolation. Agents can install packages and run
commands inside the box without approval prompts. Outbound traffic passes
through a proxy with a separate mTLS identity for each box, destination policy,
and request auditing.

Managed credentials remain outside the box. Agents receive placeholders called
sentinels; the proxy substitutes the real credential only for its bound domain.
An agent can request additional access, which a human grants with a host scope
and an expiry.

An LLM judge checks privileged credential use against grants written in English.
The judge currently runs inside the box, so it is a guardrail rather than a
security boundary against a compromised agent.

See [discobox.ai](https://discobox.ai) for the full overview and
[architecture](https://discobox.ai/architecture).

## License

See [LICENSE](LICENSE).
