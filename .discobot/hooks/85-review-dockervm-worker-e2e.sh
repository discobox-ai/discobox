#!/bin/bash
#---
# name: Docker VM worker flow e2e
# type: file
# pattern: "**/*.go"
# phase: review
#---

set -euo pipefail

DISCOBOX_DOCKER_VM_INTEGRATION=1 go test ./internal/service -run TestDockerVMProviderWorkerCreateFlowE2E -count=1 -v
