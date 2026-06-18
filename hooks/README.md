# hooks

`hooks` is the standalone Discobox hook runner module.

It will discover hook definitions from `.discobox/hooks` at the Git repository
root, start a session-scoped daemon on demand, watch file changes, run matching
hooks serially, and persist hook status/run history through the shared GORM DB
path.

Design notes live in [`DESIGN.md`](DESIGN.md). Implementation planning details
live in [`PLAN.md`](PLAN.md).
