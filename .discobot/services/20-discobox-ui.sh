#!/bin/bash
#---
# name: Discobox UI
# description: Runs the SvelteKit UI dev server on port 5173
# order: 20
# http: 5173
#---

set -euo pipefail

exec go tool task dev:ui
