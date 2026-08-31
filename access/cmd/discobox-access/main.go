// Command discobox-access is the in-sandbox client for the agent
// credentials protocol: it asks a human for a credential the agent was not
// provisioned with, and runs one command with it.
//
// It speaks only the protocol, over a base URL, so it is not tied to Discobox
// or to any one harness. See docs/agent-credentials-protocol.md.
package main

import (
	"os"

	"github.com/discobox-ai/discobox/access"
)

func main() {
	os.Exit(access.Run(os.Args[1:]))
}
