// Command discobox-docker installs as /usr/local/bin/docker, ahead of the real
// CLI on PATH, and transparently gives `docker build` a pool-shared BuildKit
// cache. Every other docker command is exec'd through untouched. See the
// dockercache package for why a PATH shim is the only injection point.
package main

import (
	"os"

	"github.com/obot-platform/discobox/sandbox-agent/dockercache"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		// Without a home directory there is nowhere to put a cache, but the
		// user's command still has to run.
		home = ""
	}
	os.Exit(dockercache.Run(os.Args[1:], home))
}
