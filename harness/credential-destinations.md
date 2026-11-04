# Built-in credential destinations

Policy for ADR 26-11-04-772 / #576. `image.json` is the executable authority;
this inventory records its evidence and limits. It covers saved/delivered
credentials, not the initial local login before handoff (#581).

## Evidence inspected 2026-11-04

Built-ins install the latest release at provisioning time, not a repository-pinned
CLI. This audit sampled Claude Code 2.1.318, OpenCode 2.2.5, pi-ai 1.5.0 and
omp pi-ai 18.16.0. Recheck authenticated destinations when upgrading a harness.
Do not infer credential routes from general network reachability.

- Claude: [official network requirements](https://code.claude.com/docs/en/network-config#network-access-requirements)
  distinguish model API, account authentication and authenticated MCP connector
  proxy from browser sign-in, refresh and download hosts. The API key is for
  `api.anthropic.com`; the subscription access token also serves `claude.ai`
  account calls and `mcp-proxy.anthropic.com` connectors. Browser and server
  refresh endpoints are not access-token spending routes.
- Codex: [OpenAI's endpoint description](https://openai.com/index/unrolling-the-codex-agent-loop/)
  distinguishes API-key traffic from ChatGPT login traffic;
  [backend source](https://github.com/openai/codex/tree/main/codex-rs/backend-client)
  also uses account/usage/model endpoints. Use exact hosts without guessed path
  or method limits. Server refresh uses registered OAuth metadata separately.
- [OpenCode v2.2.5 source](https://github.com/anomalyco/opencode/tree/v2.2.5/packages):
  `ai/src/providers`, `ai/src/protocols`, and
  `core/src/plugin/provider/{openai,github-copilot}.ts` define base URLs,
  ChatGPT routing and Copilot's GitHub API probes.
- [pi-ai 1.5.0 published source](https://registry.npmjs.org/@earendil-works/pi-ai/-/pi-ai-1.5.0.tgz):
  `dist/providers/data/*.json` supplies model API bases;
  `dist/auth/oauth/github-copilot.js` exchanges a GitHub credential at
  `/copilot_internal/v2/token`. Its derived Copilot token is a different
  credential. The source token does not need model-host permission in pi.
- [omp pi-ai 18.16.0 published source](https://registry.npmjs.org/@oh-my-pi/pi-ai/-/pi-ai-18.16.0.tgz):
  `src/providers`, `src/registry/oauth`, and `src/usage` identify OAuth provider
  hosts, Google Code Assist fallback destinations, and xAI subscription billing.
- [Perplexity's official SDK](https://github.com/perplexityai/perplexityai-typescript/blob/main/src/client.ts)
  supplies its API base. Copilot CLI keeps its existing route allowlist and
  explicit blocks, identical in static and provider metadata.

No live authenticated vendor sessions were captured in this audit. The policy
allows the source-established host surfaces; HTTP methods and paths are left
unrestricted where the source has multiple API/auxiliary operations. Tests
exercise policy and proxy delivery with synthetic values and a local origin.
Custom base URLs do not gain permission from settings or local metadata.

## Provider inventory

These provider IDs are registered by OpenCode, omp and pi. `token` includes
an unrenewable access token when that harness normalizes it as a plain token;
`oauth` means server-renewed OAuth. Their accepted login formats remain the
normalizers' responsibility. The route applies to every accepted type unless
an exception is listed. No omitted policy means “anywhere”.

| Provider | Exact credential destination hosts / exception |
| --- | --- |
| anthropic | api.anthropic.com |
| openai | api.openai.com; OpenCode OAuth instead uses chatgpt.com |
| google | generativelanguage.googleapis.com |
| google-vertex | Explicit person-owned endpoint routes required; project, location and service-account flows vary |
| groq | api.groq.com |
| mistral | api.mistral.ai |
| openrouter | openrouter.ai |
| xai | api.x.ai |
| github-copilot | OpenCode/omp use the Copilot allowlist and explicit blocks; pi source credential only permits GET api.github.com/copilot_internal/v2/token |
| deepseek | api.deepseek.com |
| cerebras | api.cerebras.ai |
| zai | api.z.ai |
| kimi-coding | api.kimi.com |
| minimax | api.minimax.io |
| huggingface | router.huggingface.co |
| azure | Explicit person-owned endpoint routes required; deployment/resource names vary |
| amazon-bedrock | Explicit person-owned endpoint routes required; regions, endpoints and signing flows vary |
| google-gemini-cli | cloudcode-pa.googleapis.com |
| google-antigravity | daily-cloudcode-pa.googleapis.com, daily-cloudcode-pa.sandbox.googleapis.com, cloudcode-pa.googleapis.com |
| openai-codex | chatgpt.com; pi's ChatGPT OAuth login remains unsupported by its existing normalizer |
| xai-oauth | api.x.ai, cli-chat-proxy.grok.com (model API and subscription usage/billing) |
| alibaba | Explicit person-owned endpoint routes required; region/workspace endpoints vary |
| togetherai | api.together.xyz |
| fireworks-ai | api.fireworks.ai |
| perplexity | api.perplexity.ai |
| cohere | api.cohere.ai |
| opencode | opencode.ai |

Claude Code and Codex CLI declare matching static/provider policies for their
API keys and OAuth credentials. Shell collects none: not applicable. Secrets a
person independently binds to any harness retain their own policy.

The Copilot allowlist permits `githubcopilot.com` and its subdomains, selected
`api.github.com` paths (including subdomains), while blocking other GitHub
repository access and non-readonly MCP/agent paths. A more specific open route
can override a block within that list, as existing route semantics define.
Do not replace it with a vendor-wide GitHub allowlist.

## Durable narrowing and custom endpoints

`Secret.routeConstraints` is a nullable JSON column of independent route lists.
Every list is ANDed with the original `routes`, bindings and delegation parent.
An empty inner list denies all requests; no outer lists adds no restriction.
Capture and bundle refresh append normalized, deduplicated policies for owned
secrets, never replace their original restrictions. Future refreshes can only
narrow these historical lists. AutoMigrate adds the column without recreating
rows. The update and holder dirty marks share the snapshot/capture transaction.
Static and provider policy share refresh handling, including numbered accounts.
Manually bound secrets are not rewritten.

This is the durable intersection form left open in the ADR. It deliberately
keeps policy layers rather than trying to combine overlapping host/path/method
patterns and blocked carve-outs into one list. API reads expose the constraints;
bound-credential listings and pool publication carry all lists.

For providers requiring explicit routes, new automatic capture protects the
local credential file and emits a fixed endpoint-policy diagnostic. The server
also refuses such a candidate independently of exporter behavior. Existing
owned credentials are held to an empty policy on refresh, rather than retaining
`*`. To restore a custom endpoint, create a person-owned secret with the exact
endpoint routes and bind it to the harness config. Its credential document must
reference that variable, as for other manually bound harness credentials.

Narrowing can disable a legacy custom destination. Inspect `routeConstraints`
on the secret when diagnosing a route refusal; a newly saved login does not
remove an old denial. Create and bind a separate person-owned secret when a
wider destination policy is intended.
