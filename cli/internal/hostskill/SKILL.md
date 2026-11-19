---
name: discobox-host
description: Use Discobox from the machine running its CLI. Use when the user asks to write, install, or troubleshoot a Discobox tool, such as opening lazygit in a box with their local configuration, or to report a problem with Discobox itself.
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

## Report a problem with Discobox

Use https://github.com/discobox-ai/discobox/issues for problems with Discobox
itself. If the user only asks for help diagnosing a problem, tell them what
you found and where they can report it.

1. Search open and closed issues first. Use general symptoms and remove
   sensitive details from search queries. If an issue already describes the
   problem, share the link and offer to add any new evidence there. If you
   cannot search, ask the user to check for duplicates.
2. Draft a report with the smallest steps that reproduce the problem, what
   you expected, what happened, and the exact error with sensitive details
   removed. Run `discobox version` on this machine and include the client and
   server versions it reports. If the server version is unavailable, say so.
   Include the host OS and architecture and any relevant box or pool details
   the user can share.
3. Leave out the user's project name, code, private paths and hostnames,
   credentials, and other sensitive details unless the user explicitly
   approves sharing the relevant information. Never include credentials.
   Show the draft to the user and get their approval before filing it or
   posting a comment. Use the user's existing GitHub access if it allows the
   write. If it does not, give them the draft and the tracker or issue link.
