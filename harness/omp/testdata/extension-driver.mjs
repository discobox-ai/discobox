// Drives hook-extension.js the way omp does — an extension factory handed a
// `pi` whose `on` registers handlers, each later called with an event and a
// context — and prints, as JSON, what reached the publisher. Run by
// TestExtensionPublishes. The publisher is the stub on PATH the test writes,
// which appends one JSON line per call to $PUBLISHED.
//
// The event shapes are omp 18.11.0's: a tool_call carries toolName, toolCallId
// and input, and a tool_result the output beside them; before_agent_start
// carries the prompt beside the whole system prompt and any pasted images;
// session_stop the whole transcript; a compaction the entries it summarizes
// (recorded from a running session, and the compaction ones from omp's
// extension types). A context names its agent as { kind: "main" | "sub", ... }.
import fs from "node:fs";
import extension from "../hook-extension.js";

const handlers = new Map();
const pi = {
  on(event, handler) {
    if (!handlers.has(event)) handlers.set(event, []);
    handlers.get(event).push(handler);
  },
};
extension(pi);

const main = { cwd: "/workspace", agent: { kind: "main", name: "main", id: "m", depth: 0 } };
const sub = { cwd: "/workspace", agent: { kind: "sub", name: "explore", id: "s", depth: 1 } };
const returned = [];
const fire = async (event, payload, ctx) => {
  for (const handler of handlers.get(event) ?? []) returned.push(await handler({ type: event, ...payload }, ctx));
};

// Every main-only event is fired for a subagent first and then for the main
// session, so a count proves both that the subagent's copy is dropped and that
// the main session's still gets through.
for (const ctx of [sub, main]) {
  await fire("session_start", {}, ctx);
  await fire("before_agent_start", { prompt: "fix it", systemPrompt: ["huge"], images: [{ type: "image", data: "base64" }] }, ctx);
  await fire("tool_call", { toolName: "bash", toolCallId: "c1", input: { command: "ls" } }, ctx);
  await fire("tool_result", { toolName: "bash", toolCallId: "c1", input: { command: "ls" }, content: [{ type: "text", text: "out" }], details: {}, isError: false }, ctx);
  await fire("session_before_compact", { preparation: { messagesToSummarize: [{ role: "user" }], tokensBefore: 90000 }, branchEntries: [{ id: "e1" }], signal: {} }, ctx);
  await fire("session_compact", { compactionEntry: { summary: "everything so far" }, fromExtension: false }, ctx);
  await fire("session_stop", { messages: [{ role: "user" }] }, ctx);
  await fire("session_shutdown", {}, ctx);
}
// Stream-rate events are never published.
await fire("message_update", { message: {} }, main);
await fire("tool_execution_update", {}, main);

const published = fs.existsSync(process.env.PUBLISHED)
  ? fs.readFileSync(process.env.PUBLISHED, "utf8").trim().split("\n").filter(Boolean).map((line) => JSON.parse(line))
  : [];
console.log(JSON.stringify({ registered: [...handlers.keys()], published, returned }));
