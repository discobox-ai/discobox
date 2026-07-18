#!/bin/bash
#---
# name: Docker pool flow e2e
# type: file
# pattern: "**/*.go"
# phase: review
#---

set -euo pipefail

(cd server && DISCOBOX_DOCKER_INTEGRATION=1 go test ./internal/service -run TestDockerProviderPoolCreateFlowE2E -count=1 -v)
