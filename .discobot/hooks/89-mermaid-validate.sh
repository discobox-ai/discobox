#!/bin/bash
#---
# name: Mermaid diagram validation
# type: file
# pattern: "**/*.md"
#---

set -euo pipefail

go tool task check:mermaid
