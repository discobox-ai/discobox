package server

import (
	"context"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
)

func (*routerTestServices) GetSandboxHarnessConfigChanges(context.Context, string, string) (harnesscapture.Preview, error) {
	return harnesscapture.Preview{}, nil
}
func (*routerTestServices) SaveSandboxHarnessConfigChanges(context.Context, string, string, harnesscapture.CandidateRequest) (harnesscapture.SaveResult, error) {
	return harnesscapture.SaveResult{}, nil
}
func (*routerTestServices) ReconsiderSandboxHarnessConfigChanges(context.Context, string, string, harnesscapture.Selection) (harnesscapture.Preview, error) {
	return harnesscapture.Preview{}, nil
}
func (*routerTestServices) DiscardSandboxHarnessConfigChanges(context.Context, string, string, harnesscapture.CandidateRequest) (harnesscapture.Preview, error) {
	return harnesscapture.Preview{}, nil
}
