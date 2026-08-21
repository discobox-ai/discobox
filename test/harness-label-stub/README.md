# Label-only harness stub image

`go tool task ci:harness-label-images` builds one of these per built-in harness
and tags it `discobox-harness-<slug>:ci`. Each carries that harness's real
`image.json` as its `io.discobox.image.v1` label and nothing else — no
filesystem, no sandbox agent, no harness.

That is all built-in harness seeding reads, so pointing
`DISCOBOX_HARNESS_<SLUG>_IMAGE` at them lets the unit test suite run on a
machine that has never built the real images (ADR 0066 §7). `go tool task
ci:test` does exactly that.

These images cannot start a sandbox. Anything that runs one needs the real
images from `go tool task build:harness-images`, or the stub harness in
[`../harness-stub`](../harness-stub), which is a working harness built on the
sandbox agent base.
