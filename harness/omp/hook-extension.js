// Publishes omp's lifecycle events to the Discobox sandbox agent.
//
// The claude-code and codex images declare their hooks as commands in a
// managed settings file and the CLI runs them. omp has no command hook: its
// lifecycle reaches an extension and nothing else, so this extension is the
// equivalent unit — image-owned, version-coupled to the CLI it ships beside,
// and loaded by the harness launcher with --hook, because an `extensions` list
// in omp's configuration overlay would replace the user's rather than add to
// it.
//
// It publishes through `discobox-hook-publish`, the same generic publisher the
// other images' hooks invoke, so the wire protocol has one implementation and
// this file never learns it. The publisher takes its payload on stdin and
// reads the terminal ID and socket path from the environment omp already runs
// in.

import { spawn } from "node:child_process";

const PROVIDER = "omp";

// Events worth recording, named exactly as omp emits them.
//
// An allowlist, not a denylist. omp's runtime carries per-token events —
// `message_update` fires for every delta of every message — and publishing
// one spawns a process and writes a row. A denylist would publish each new
// event omp ships by default, and a stream-rate one would flood a sandbox's
// hook table until somebody noticed; an allowlist fails the other way, by
// missing a new event until it is added here. That makes omp's trail coarser
// than Claude Code's on purpose: it records a turn's shape and not its content.
//
// omp's subagents are processes of their own, launched without this hook, so
// every event here is the root session's.
const PUBLISHED_EVENTS = new Set([
  "session_start",
  "session_switch",
  "session_shutdown",
  "before_agent_start",
  "agent_start",
  "agent_end",
  "session_stop",
  "turn_start",
  "turn_end",
  "tool_call",
  "tool_result",
  "tool_approval_requested",
  "tool_approval_resolved",
  "session_before_compact",
  "session_compact",
  "credential_disabled",
]);

// What of each event is published. A hook row is a record of what happened,
// not a copy of the conversation: a tool's input says what it was asked to do,
// its result is the file it read; a prompt is what the user asked, the
// messages an agent run produced are the transcript. So the payload is the
// event's scalar fields, plus the one structured field that names the
// event's subject.
function payloadOf(event) {
  const payload = {};
  for (const [key, value] of Object.entries(event ?? {})) {
    if (key === "type") continue;
    if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
      payload[key] = value;
    }
  }
  if ((event?.type === "tool_call" || event?.type === "tool_result") && event.input && typeof event.input === "object") {
    payload.input = event.input;
  }
  return payload;
}

// publish runs the publisher with the payload on its stdin — redirected rather
// than interpolated: a payload carries arbitrary text, a prompt or a command
// line, and stdin is the one path that cannot be confused for shell syntax.
// A harness must run whether or not its hooks are recorded, so nothing here
// throws, and a publisher that is missing or refuses is the log's problem,
// never the session's.
function publish(event, payload) {
  return new Promise((resolve) => {
    let child;
    try {
      child = spawn("discobox-hook-publish", ["--provider", PROVIDER, "--event", event], {
        stdio: ["pipe", "ignore", "ignore"],
      });
    } catch {
      resolve();
      return;
    }
    child.on("error", () => resolve());
    child.on("close", () => resolve());
    child.stdin.on("error", () => {});
    child.stdin.end(JSON.stringify(payload ?? {}));
  });
}

export default function (pi) {
  // Nothing to publish to when the sandbox agent is not listening — a
  // configure sandbox, or omp run outside a Discobox terminal. The publisher
  // tolerates it, but spawning a process per event to discover that does not.
  if (!process.env.DISCOBOX_HOOK_SOCKET) return;
  for (const event of PUBLISHED_EVENTS) {
    pi.on(event, async (e) => {
      await publish(event, payloadOf(e));
      // Returning nothing leaves every event's behaviour unchanged: a
      // tool_call is not blocked, a tool_result is not rewritten, a
      // session_stop asks for no continuation.
    });
  }
}
