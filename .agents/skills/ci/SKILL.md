---
allowed-tools: Bash(go tool task:*), Edit, Read, Glob, Grep
description: Run the checks CI runs and resolve all issues
---

Run the three targets CI runs, in this order, and resolve everything they print:

```bash
go tool task ci:check
go tool task ci:test
go tool task verify
```

They are the Linux half of `.github/workflows/ci.yml`. The macOS
(`ci:darwin`) and Windows halves need those runners; note anything the change
plausibly affects there instead of guessing.
