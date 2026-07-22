#!/bin/bash
#---
# name: Mermaid diagram validation
# type: file
# pattern: "**/*.md"
#---

set -euo pipefail

# check:mermaid parses every diagram in-process with mermaid's own grammars, so
# there is no browser to detect or launch.
go tool task check:mermaid
