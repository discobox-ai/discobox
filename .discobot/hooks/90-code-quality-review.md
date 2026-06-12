---
name: Code quality review
type: file
engine: ai
pattern: "**/*"
description: Review changed code against workspace guidance before completion
phase: review
---
Perform a code quality review of the changed files for this hook run.

First, find and read any applicable workspace guidance files, including:

- AGENT.md or AGENTS.md
- REVIEW.md or REVIEWS.md
- GUIDELINE.md or GUIDELINES.md

Search from the workspace root and within relevant subdirectories. Apply the
most specific guidance for each changed file, with nested guidance taking
precedence over broader guidance when they conflict.

Review the changes for:

- Compliance with the guidance files listed above.
- Correctness, maintainability, readability, and simplicity.
- Idiomatic use of the relevant language, framework, and project patterns.
- Missing or inadequate tests for behavior changes.
- Reliability or concurrency issues.
- Unnecessary scope expansion, speculative abstractions, or unrelated changes.

Focus on actionable, high-signal findings. Ignore purely subjective style
preferences unless they are required by the guidance files or project patterns.
