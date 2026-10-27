// Publishes opencode's lifecycle events to the Discobox sandbox agent.
//
// The other harness images declare their hooks as commands in a managed
// settings file and the CLI runs them. opencode has no command hook: its
// lifecycle reaches plugins and nothing else, so this plugin is the equivalent
// unit — image-owned, version-coupled to the CLI it ships beside, and named by
// opencode's managed config layer (`/etc/opencode`) rather than by the user's
// own config, which the configure flow replaces (ADR 0147).
//
// It is an opencode 2 plugin (ADR 26-10-15-848): a directory whose `server`
// module default-exports `{id, setup}`. opencode resolves a plugin path to a
// directory and refuses a single file.
//
// It publishes through `discobox-hook-publish`, the same generic publisher the
// other images' hooks invoke, so the wire protocol has one implementation and
// this file never learns it. The publisher takes its payload on stdin and reads
// the terminal ID and socket path from the environment opencode runs in — which
// is the launching terminal's own, because the harness runs opencode
// `--standalone` rather than through a background service another terminal
// started.

import { spawn } from "node:child_process";

const PROVIDER = "opencode";

// Bus events worth recording, named exactly as opencode emits them.
//
// An allowlist, not a denylist. opencode's bus carries per-token streaming
// events — `session.text.delta` fires for every delta of every message — and
// publishing one spawns a process and writes a row. A denylist would publish
// each new event opencode ships by default, and a stream-rate one would flood
// a sandbox's hook table until somebody noticed; an allowlist fails the other
// way, by missing a new event until it is added here.
//
// Tool calls are deliberately absent: they are published from the tool
// domain's `execute.before` / `execute.after` hooks, which carry the call's
// input, rather than from the bus.
const PUBLISHED_EVENTS = new Set([
  "session.created",
  "session.deleted",
  "session.execution.started",
  "session.execution.succeeded",
  "session.execution.failed",
  "session.execution.interrupted",
  "session.compaction.ended",
  "permission.asked",
  "permission.replied",
]);

// The session lifecycle events with a canonical name, which therefore mean the
// root session's here. opencode's subagent tool runs a child session, and each
// one publishes its own `session.created`, `session.execution.succeeded`,
// `session.execution.failed` and `session.compaction.ended`. A wait matches by
// name and the name carries no session, so publishing a child's success would
// end a `--hook Stop` at the first subagent to finish, which is the false
// turn-end ADR 0137's matcher exists to avoid.
//
// `session.deleted` is deliberately NOT here. It has no canonical name, so it
// cannot be mistaken for the root's turn ending, and a child's deletion is a
// fact worth recording under opencode's own name — which is what ADR 0146 §3
// keeps un-canonical events around for. It is also what bounds the set below.
const ROOT_ONLY_EVENTS = new Set([
  "session.created",
  "session.execution.succeeded",
  "session.execution.failed",
  "session.compaction.ended",
]);

// publish runs the publisher with the payload on stdin. A payload carries
// arbitrary text — a prompt, a diff, a command line — and stdin is the one path
// that cannot be confused for an argument. It resolves however the publisher
// fares: a harness must run whether or not its hooks are recorded.
function publish(event, payload) {
  return new Promise((resolve) => {
    try {
      const child = spawn("discobox-hook-publish", ["--provider", PROVIDER, "--event", event], {
        stdio: ["pipe", "ignore", "ignore"],
      });
      child.on("error", () => resolve());
      child.on("close", () => resolve());
      child.stdin.on("error", () => {});
      child.stdin.end(JSON.stringify(payload ?? {}));
    } catch {
      resolve();
    }
  });
}

export default {
  id: "discobox-hooks",
  setup: async ({ event, tool }) => {
    // Nothing to publish to when the sandbox agent is not listening — a
    // configure sandbox, or opencode run outside a Discobox terminal. The
    // publisher tolerates it, but spawning a process per event to discover
    // that does not.
    if (!process.env.DISCOBOX_HOOK_SOCKET) return;

    // Sessions known to have a parent, learned from the `session.created`
    // that announced them. A session not in this set is treated as the root,
    // which is the safe default: on a resumed session the root's own
    // `session.created` fired in an earlier run and will not be seen again,
    // while any child created during this one announces itself here first.
    const childSessions = new Set();

    // opencode names these before and after a tool runs; they are what a
    // reader waiting on tool use matches, and they carry the canonical
    // PreToolUse and PostToolUse names. `tool` and `args` are the shape the
    // audit summary reads (cli/internal/cli/audit_list.go).
    await tool.hook("execute.before", async (call) => {
      await publish("tool.execute.before", {
        tool: call?.tool,
        sessionID: call?.sessionID,
        callID: call?.id,
        args: call?.input,
      });
    });
    await tool.hook("execute.after", async (call) => {
      await publish("tool.execute.after", {
        tool: call?.tool,
        sessionID: call?.sessionID,
        callID: call?.id,
        args: call?.input,
        status: call?.status,
        error: call?.status === "error" ? call?.error?.message : undefined,
      });
    });

    // Not awaited: the subscription lasts as long as opencode does, and setup
    // has to return for opencode to start. Each event is published before the
    // next is read, so the trail keeps the bus's order.
    (async () => {
      for await (const e of event.subscribe()) {
        if (!e?.type || !PUBLISHED_EVENTS.has(e.type)) continue;
        const data = e.data ?? {};
        const sessionID = data.sessionID;
        if (e.type === "session.created" && data.parentID && sessionID) {
          childSessions.add(sessionID);
        }
        // A deleted session can never be spoken of again, so this is also
        // what keeps the set from growing for the life of the terminal.
        if (e.type === "session.deleted" && sessionID) {
          childSessions.delete(sessionID);
        }
        if (ROOT_ONLY_EVENTS.has(e.type) && sessionID && childSessions.has(sessionID)) {
          continue;
        }
        await publish(e.type, data);
      }
    })().catch(() => {
      // The bus closing ends the subscription; there is nothing to recover.
    });
  },
};
