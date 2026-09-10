package terminal

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/judge"
)

func TestOrdinarySandboxCannotServeJudgeJobs(t *testing.T) {
	service := newHarnesslessService(t)
	if verdict, err := service.Judge(context.Background(), judge.Job{Kind: "command", Purpose: "open PR", Host: "github.com", Command: []string{"gh", "pr", "create"}}); err == nil || verdict.Allow {
		t.Fatal("work sandbox served a trusted verdict")
	}
}
