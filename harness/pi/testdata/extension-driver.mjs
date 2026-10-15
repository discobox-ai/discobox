// Drives hook-extension.js the way pi does — an extension factory handed a
// `pi` whose `on` registers handlers, each later called with an event and a
// context — and prints, as JSON, what reached the publisher. Run by
// TestExtensionPublishes. The publisher is the stub on PATH the test writes,
// which appends one JSON line per call to $PUBLISHED.
//
// The event shapes are pi 1.2.0's, recorded from a running session where it
// could be (session_start, before_agent_start, tool_call, tool_result,
// agent_settled, session_shutdown) and taken from pi's extension types
// otherwise. Each carries what pi carries beside the fields worth recording:
// before_agent_start the whole system prompt as a string and its structured
// options, tool_result the tool's output, a compaction the entries it
// summarizes. pi has no subagents, and a context carries no agent.
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

const ctx = { cwd: "/workspace", mode: "tui", hasUI: true, sessionManager: { entries: ["huge"] } };
const returned = [];
const fire = async (event, payload) => {
  for (const handler of handlers.get(event) ?? []) returned.push(await handler({ type: event, ...payload }, ctx));
};

await fire("session_start", { reason: "startup" });
await fire("input", { text: "fix it", images: [], source: "interactive" });
await fire("before_agent_start", {
  messageType: "user",
  prompt: "fix it",
  images: [{ type: "image", data: "base64" }],
  systemPrompt: "You are an expert coding assistant operating inside pi, a coding agent harness. SECRET-SYSTEM-PROMPT",
  systemPromptOptions: { sections: ["huge"] },
});
await fire("agent_start", {});
await fire("turn_start", { turnIndex: 0, timestamp: 1 });
await fire("tool_execution_start", { toolCallId: "c1", toolName: "bash", args: { command: "ls" } });
await fire("tool_call", { toolName: "bash", toolCallId: "c1", input: { command: "ls" } });
await fire("tool_result", {
  toolName: "bash", toolCallId: "c1", input: { command: "ls" },
  content: [{ type: "text", text: "SECRET-TOOL-OUTPUT" }], details: { truncated: false }, structuredContent: null, isError: false, usage: {},
});
await fire("tool_execution_end", { toolCallId: "c1", toolName: "bash", result: {}, isError: false, durationMs: 15 });
await fire("message_update", { message: {}, assistantMessageEvent: {} });
await fire("tool_execution_update", { toolCallId: "c1", toolName: "bash", args: {}, partialResult: {} });
await fire("turn_end", { turnIndex: 0, message: { content: "SECRET-TRANSCRIPT" }, toolResults: [], outcome: "completed", continue: false });
await fire("agent_end", { messages: [{ role: "user", content: "SECRET-TRANSCRIPT" }] });
await fire("session_before_compact", {
  preparation: { messagesToSummarize: [{ content: "SECRET-TRANSCRIPT" }] }, branchEntries: [{ id: "e1" }],
  customInstructions: "keep the plan", reason: "threshold", willRetry: false, signal: {},
});
await fire("session_compact", { compactionEntry: { summary: "SECRET-TRANSCRIPT" }, fromExtension: false, reason: "threshold", willRetry: false });
await fire("session_compact_failed", { reason: "overflow", errorMessage: "too long", aborted: false, willRetry: true, fromExtension: false });
await fire("agent_settled", { aborted: false });
await fire("session_shutdown", { reason: "quit" });

const published = fs.existsSync(process.env.PUBLISHED)
  ? fs.readFileSync(process.env.PUBLISHED, "utf8").trim().split("\n").filter(Boolean).map((line) => JSON.parse(line))
  : [];
console.log(JSON.stringify({ registered: [...handlers.keys()], published, returned }));
