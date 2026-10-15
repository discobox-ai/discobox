// Publishes pi's lifecycle events to the Discobox sandbox agent.
//
// pi has no command hook: its lifecycle reaches JavaScript extensions and
// nothing else, so this module is what claude-code's and Copilot's images
// declare as commands. pi has no system layer to name it from either, so the
// image's harness files put a one-line re-export of it in
// ~/.pi/agent/extensions, a directory pi adds to the user's own extensions
// rather than replacing them (ADR 26-10-15-259 §3).
//
// It publishes through `discobox-hook-publish`, the same generic publisher the
// other images' hooks invoke, so the wire protocol has one implementation and
// this file never learns it. The publisher takes its payload on stdin and reads
// the terminal ID and socket path from the environment pi already runs in.
import { spawn } from "node:child_process";

const PROVIDER = "pi";

// The events worth recording, named exactly as pi emits them, each with the
// fields of its payload that are published.
//
// An allowlist of events, not a denylist: pi emits per-token events
// (message_update, tool_execution_update) and publishing one spawns a process
// and writes a row. A denylist would publish each new event pi ships by
// default; an allowlist fails the other way, by missing one until it is added
// here.
//
// And an allowlist of fields, named for each event, rather than a projection
// by shape: pi's payloads carry the conversation under many names, and
// before_agent_start carries the whole system prompt as a plain string, which
// a projection to scalar fields would publish. A hook row records the turn's
// shape — the prompt, the tool and what it was asked to do, a reason — and the
// transcript is the session's.
const PUBLISHED_FIELDS = {
  session_start: ["reason"],
  session_shutdown: ["reason"],
  before_agent_start: ["messageType", "prompt"],
  tool_call: ["toolName", "toolCallId", "parentToolCallId", "input"],
  tool_result: ["toolName", "toolCallId", "parentToolCallId", "input", "isError"],
  session_before_compact: ["reason", "willRetry"],
  session_compact: ["reason", "willRetry", "fromExtension"],
  session_compact_failed: ["reason", "aborted", "willRetry", "errorMessage"],
  agent_settled: ["aborted"],
};

// How long a publish may hold pi up. tool_call handlers are awaited before the
// tool runs, so this is latency on every tool call when the publisher is slow,
// and a publish that never returns would be a tool that never runs.
const PUBLISH_TIMEOUT_MS = 3000;

function project(event, payload, ctx) {
  const out = {};
  for (const field of PUBLISHED_FIELDS[event]) {
    if (payload?.[field] !== undefined) out[field] = payload[field];
  }
  if (typeof ctx?.cwd === "string") out.cwd = ctx.cwd;
  return out;
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
  // configure sandbox, or pi run outside a Discobox terminal. The publisher
  // tolerates it, but spawning a process per event to discover that does not.
  if (!process.env.DISCOBOX_HOOK_SOCKET) return;
  for (const event of Object.keys(PUBLISHED_FIELDS)) {
    pi.on(event, async (payload, ctx) => {
      try {
        await publish(event, project(event, payload, ctx));
      } catch {
        // A harness must run whether or not its hooks are recorded. This is the
        // log, never the behaviour.
      }
      // Nothing returned: several of these events let a handler block, rewrite
      // or continue, and recording a hook never does.
      return undefined;
    });
  }
}
