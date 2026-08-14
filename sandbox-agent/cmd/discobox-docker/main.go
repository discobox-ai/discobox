// Command discobox-docker installs as /usr/local/bin/docker, ahead of the real
// CLI on PATH, and transparently points `docker build` at the pool-shared
// BuildKit builder. Every other docker command is exec'd through untouched. See the
// dockercache package for why a PATH shim is the only injection point.
package main

import (
	"os"

	"github.com/obot-platform/discobox/sandbox-agent/dockercache"
)

func main() {
	os.Exit(dockercache.Run(os.Args[1:]))
}
