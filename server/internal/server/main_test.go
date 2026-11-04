package server

import (
	"os"
	"testing"

	"github.com/discobox-ai/discobox/server/providers/libkrun"
)

func TestMain(m *testing.M) {
	libkrun.RunLauncherIfInvoked()
	os.Exit(m.Run())
}
