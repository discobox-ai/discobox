// Drives hook-plugin.js with synthetic events and prints, as JSON, the event
// names it published. Run by TestPluginPublishesTheRightEvents.
//
// The event shapes here are the ones opencode really uses, not invented: a
// session.created payload observed from a running opencode 1.18.32 carries
// both `sessionID` and a nested `info` object, and opencode's own `run`
// command reads a child's parent as `properties.info.parentID`.
import plugin from "../hook-plugin.js";

process.env.DISCOBOX_HOOK_SOCKET = "/tmp/driver.sock";

const published = [];
// Stands in for Bun's shell. The plugin calls
// $`discobox-hook-publish --provider ${PROVIDER} --event ${event} < ${body}`,
// so the interpolated values are [provider, event, payload].
const $ = (_strings, ...values) => ({
  quiet: () => ({ nothrow: async () => { published.push(String(values[1])); } }),
});

const hooks = await plugin({ $ });
const fire = (type, properties) => hooks.event({ event: { type, properties } });

const ROOT = "ses_root";
const CHILD = "ses_child";

// Every session-scoped event is fired for BOTH the root and a child. Firing
// one only for a child would let "no child copy" pass just as well if the
// plugin stopped publishing it altogether, leaving a canonical name dead.
await fire("session.created", { sessionID: ROOT, info: { id: ROOT } });
await hooks["tool.execute.before"]({ tool: "bash", sessionID: ROOT, callID: "c1" }, { args: {} });
await fire("session.created", { sessionID: CHILD, info: { id: CHILD, parentID: ROOT } });
await fire("session.idle", { sessionID: CHILD });
await fire("session.error", { sessionID: CHILD, error: { name: "x" } });
await fire("session.compacted", { sessionID: CHILD });
await hooks["tool.execute.after"]({ tool: "bash", sessionID: CHILD, callID: "c2" }, { title: "t" });
await fire("session.compacted", { sessionID: ROOT });
await fire("session.error", { sessionID: ROOT, error: { name: "x" } });
await fire("session.idle", { sessionID: ROOT });
await fire("message.part.updated", { sessionID: ROOT });
await fire("todo.updated", { sessionID: CHILD });
await fire("session.deleted", { sessionID: CHILD });
// After its deletion the child is forgotten, so an idle naming it again is
// treated as the root's. Nothing real emits this; it is how the eviction that
// bounds the set is observable from outside.
await fire("session.idle", { sessionID: CHILD });

console.log(JSON.stringify(published));
