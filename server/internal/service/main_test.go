package service

import (
	"fmt"
	"os"
	"testing"

	"github.com/discobox-ai/discobox/server/providers/libkrun"
)

// Default selection probes the host by re-executing the server.
func TestMain(m *testing.M) {
	if len(os.Args) == 5 && os.Args[1] == "__pool-vm-launcher" && os.Args[2] == "--check-host" {
		switch os.Getenv("DISCOBOX_TEST_LIBKRUN_HOST") {
		case "ready":
			os.Exit(0)
		case "unavailable":
			fmt.Fprintln(os.Stderr, "KVM unavailable")
			os.Exit(1)
		}
	}
	libkrun.RunLauncherIfInvoked()
	os.Exit(m.Run())
}
