// Publishes omp's lifecycle events to the Discobox sandbox agent.
//
// omp has no command hook: its lifecycle reaches JavaScript extensions and
// nothing else, so this module is what claude-code's and Copilot's images
// declare as commands. omp has no system layer to name it from either, so the
// image's harness files put a one-line re-export of it in
// ~/.omp/agent/extensions, a directory omp adds to the user's own extensions
// rather than replacing them (ADR 26-10-14-718 §3).
//
// It publishes through `discobox-hook-publish`, the same generic publisher the
// other images' hooks invoke, so the wire protocol has one implementation and
// this file never learns it. The publisher takes its payload on stdin and reads
// the terminal ID and socket path from the environment omp already runs in.
import { spawn } from "node:child_process";

const PROVIDER = "omp";

// The events worth recording, named exactly as omp emits them.
//
// An allowlist, not a denylist: omp emits per-token events (message_update,
// tool_execution_update) and publishing one spawns a process and writes a row.
// A denylist would publish each new event omp ships by default; an allowlist
// fails the other way, by missing one until it is added here.
const PUBLISHED_EVENTS = new Set([
  "session_start",
  "session_shutdown",
  "before_agent_start",
  "tool_call",
  "tool_result",
  "tool_approval_requested",
  "session_before_compact",
  "session_compact",
  "session_stop",
  "before_subagent_spawn",
  "auto_retry_start",
  "credential_disabled",
]);

// The events that mean the main session's here. omp rebinds every extension
// into each subagent session it runs (the task tool, eval's agent()), and a
// subagent's session_start or compaction is indistinguishable by name from the
// user's. A wait matches by name and the name carries no session, so a
// subagent's copy would end a `--hook SessionStart` wait early. session_stop
// is the main session's by omp's own definition and is filtered anyway, so
// its canonical name does not rest on that promise alone.
//
// Tool events are deliberately absent: a subagent's tool call is a tool call,
// as it is for Claude Code's PreToolUse.
const MAIN_ONLY_EVENTS = new Set([
  "session_start",
  "session_shutdown",
  "before_agent_start",
  "session_before_compact",
  "session_compact",
  "session_stop",
]);

// What of an event payload is published: its scalar fields — a prompt, a tool
// name, a call id, isError, a reason — and a tool event's input, which is what
// says what the tool was asked to do. An allowlist by shape, as the events are
// one by name: omp's payloads carry the conversation under many names
// (before_agent_start the whole system prompt and any pasted images,
// session_stop the transcript, a compaction the entries it summarizes,
// tool_result the tool's output), and a field omp adds later is one more. A
// hook row records the turn's shape; the transcript is the session's.
const OBJECT_FIELDS_KEPT = new Set(["input"]);

// How long a publish may hold omp up. tool_call handlers are awaited before
// the tool runs, so this is latency on every tool call when the publisher is
// slow, and a publish that never returns would be a tool that never runs.
const PUBLISH_TIMEOUT_MS = 3000;

function project(event, ctx) {
  const payload = {};
  for (const [key, value] of Object.entries(event ?? {})) {
    const scalar = value === null || ["string", "number", "boolean"].includes(typeof value);
    if (scalar || OBJECT_FIELDS_KEPT.has(key)) payload[key] = value;
  }
  const agent = ctx?.agent;
  if (agent) payload.agent = { kind: agent.kind, name: agent.name, id: agent.id, depth: agent.depth };
  if (typeof ctx?.cwd === "string") payload.cwd = ctx.cwd;
  return payload;
}

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
    const timer = setTimeout(() => {
      child.kill();
      resolve();
    }, PUBLISH_TIMEOUT_MS);
    const done = () => {
      clearTimeout(timer);
      resolve();
    };
    child.on("error", done);
    child.on("close", done);
    // Written to stdin rather than passed as an argument: a payload carries
    // arbitrary text — a prompt, a command line — and stdin is the one path
    // that cannot be confused for anything else.
    child.stdin.on("error", () => {});
    let body;
    try {
      body = JSON.stringify(payload);
    } catch {
      body = "{}";
    }
    child.stdin.end(body);
  });
}

export default function discoboxHooks(pi) {
  // Nothing to publish to when the sandbox agent is not listening — a
  // configure sandbox, or omp run outside a Discobox terminal. The publisher
  // tolerates it, but spawning a process per event to discover that does not.
  if (!process.env.DISCOBOX_HOOK_SOCKET) return;
  for (const event of PUBLISHED_EVENTS) {
    pi.on(event, async (payload, ctx) => {
      try {
        if (MAIN_ONLY_EVENTS.has(event) && ctx?.agent?.kind === "sub") return;
        await publish(event, project(payload, ctx));
      } catch {
        // A harness must run whether or not its hooks are recorded. This is the
        // log, never the behaviour.
      }
      // Nothing returned: every one of these events lets a handler block,
      // rewrite or continue, and recording a hook never does.
      return undefined;
    });
  }
}
