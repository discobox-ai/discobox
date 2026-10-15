// Drives hook-plugin/server.js with synthetic events and prints, as JSON, the
// event names it published. Run by TestPluginPublishesTheRightEvents.
//
// The shapes here are the ones opencode 2 really uses, not invented: recorded
// from opencode 2.0.29 by a plugin logging every bus event and tool hook, a
// bus event is {type, data} with the session in data.sessionID, a child
// session's session.created carries data.parentID, and a tool hook receives
// {tool, sessionID, id, input}.
//
// Publishing is the real code path: the plugin spawns discobox-hook-publish,
// and this driver puts a stub of it first on PATH that appends the event name
// to a file.
import { mkdtempSync, writeFileSync, readFileSync, chmodSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import plugin from "../hook-plugin/server.js";

const dir = mkdtempSync(join(tmpdir(), "opencode-plugin-driver-"));
const log = join(dir, "published");
writeFileSync(log, "");
const stub = join(dir, "discobox-hook-publish");
writeFileSync(stub, `#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' "$4" >> '${log}'\n`);
chmodSync(stub, 0o755);
process.env.PATH = `${dir}:${process.env.PATH}`;
process.env.DISCOBOX_HOOK_SOCKET = join(dir, "hook.sock");

const ROOT = "ses_root";
const CHILD = "ses_child";

const hooks = {};
const ev = (type, data) => ({ event: { id: `evt_${type}`, type, data } });
const call = (name, input) => ({ hook: name, input });

// Every session-scoped event is fired for BOTH the root and a child. Firing
// one only for a child would let "no child copy" pass just as well if the
// plugin stopped publishing it altogether, leaving a canonical name dead.
const script = [
  ev("session.created", { sessionID: ROOT }),
  ev("session.execution.started", { sessionID: ROOT }),
  call("execute.before", { tool: "shell", sessionID: ROOT, id: "c1", input: { command: "ls" } }),
  ev("session.created", { sessionID: CHILD, parentID: ROOT }),
  ev("session.execution.succeeded", { sessionID: CHILD }),
  ev("session.execution.failed", { sessionID: CHILD, error: { type: "x", message: "x" } }),
  ev("session.compaction.ended", { sessionID: CHILD }),
  call("execute.after", { tool: "shell", sessionID: CHILD, id: "c2", input: { command: "ls" }, status: "completed" }),
  ev("session.compaction.ended", { sessionID: ROOT }),
  ev("session.execution.failed", { sessionID: ROOT, error: { type: "x", message: "x" } }),
  ev("session.execution.succeeded", { sessionID: ROOT }),
  ev("session.text.delta", { sessionID: ROOT, delta: "hi" }),
  ev("session.tool.called", { sessionID: CHILD }),
  ev("session.deleted", { sessionID: CHILD }),
  // After its deletion the child is forgotten, so a success naming it again
  // is treated as the root's. Nothing real emits this; it is how the eviction
  // that bounds the set is observable from outside.
  ev("session.execution.succeeded", { sessionID: CHILD }),
];

let finished;
const done = new Promise((resolve) => (finished = resolve));
// The plugin reads the next event only after publishing the last, so a tool
// call made here, between two events, lands between them in the trail.
async function* subscribe() {
  try {
    for (const step of script) {
      if (step.hook) {
        await hooks[step.hook](step.input);
        continue;
      }
      yield step.event;
    }
  } finally {
    finished();
  }
}

await plugin.setup({
  event: { subscribe },
  tool: {
    hook: async (name, callback) => {
      hooks[name] = callback;
      return { dispose: async () => {} };
    },
  },
});
await done;
// The loop publishes the last event after the generator's final pull has
// returned it, so wait for the trail to stop growing.
let previous = -1;
for (;;) {
  await new Promise((resolve) => setTimeout(resolve, 50));
  const size = readFileSync(log, "utf8").length;
  if (size === previous) break;
  previous = size;
}

const published = readFileSync(log, "utf8").split("\n").filter(Boolean);
rmSync(dir, { recursive: true, force: true });
console.log(JSON.stringify(published));
