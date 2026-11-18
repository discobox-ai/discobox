---
name: discobox-host
description: Use Discobox from the machine running its CLI. Use when the user asks to write, install, or troubleshoot a Discobox tool, such as opening lazygit in a box with their local configuration.
metadata:
  discobox:
    audience: host
    version: "CLI_VERSION"
---

If `/etc/discobox/sandbox.json` exists, you are inside a box. Use the box's
own `discobox` skill instead of this one.

Compare this skill's version with the client version printed by
`discobox version --client`. If they differ, read `discobox skill` for the
running CLI's instructions. Refresh the installed copy with
`discobox skill --install` using the scope or directories where it was installed.

Discobox provides Linux sandboxes for development. The CLI runs on this
machine and can start a box and run programs in its working tree. The host
and box can have different operating systems or CPU architectures.

## Write a user tool

1. Read `discobox tools --help`. It gives the tool directory for this machine,
   declaration formats, host script environment, and a worked example.
2. Write the tool in the directory shown in the help. Prefer a host script
   when the tool needs to copy the user's local configuration into the box.
   Copy configuration only. Have the box download its own executable for its
   OS and architecture and cache it there by version. Follow the help's
   example for verifying the download.
3. Check the declaration with `discobox tools ls`. Read any reason it gives
   for why the tool cannot run.
4. Create a disposable test box using `discobox run` so you can test setup
   without touching an existing box. Run `discobox tools <id> <box>` from a
   terminal and check that the program opens in the working tree with the
   intended configuration. Try a second run if the script copies configuration
   or installs software. Remove the test box with `discobox rm` afterward.
