package terminal

import (
	"context"
	"fmt"
	"time"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// Judge serves a fresh, non-interactive verdict from a dedicated runtime.
func (s *Service) Judge(ctx context.Context, job judge.Job) (judge.Verdict, error) {
	if s.harnessMode != judge.Mode {
		return judge.Verdict{}, fmt.Errorf("this sandbox is not a judge runtime")
	}
	prompt, err := judge.Prompt(job)
	if err != nil {
		return judge.Verdict{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, judge.Timeout)
	defer cancel()
	select {
	case s.judgeSlot <- struct{}{}:
		defer func() { <-s.judgeSlot }()
	default:
		return judge.Verdict{}, fmt.Errorf("judge is busy")
	}
	env := execs.EnvWithRuntimeDefaults(execs.MergeEnv(s.env, s.exportedSecretEnv()), s.defaultUser)
	dir, err := s.execs.DefaultWorkdir()
	if err != nil {
		return judge.Verdict{}, err
	}
	if err := s.installer.EnsureInstalled(ctx, s.harness, dir, env); err != nil {
		return judge.Verdict{}, err
	}
	v := judge.Verdict{Role: judge.Role, Prompt: prompt, PromptVersion: judge.PromptVersion}
	start := time.Now()
	out, err := s.execs.RunOneShot(ctx, []string{"/usr/local/bin/discobox-prompt", "--model", judge.Role, "--system", judge.System, "--prompt", prompt, "--output-schema", judge.Schema, "--no-tools"}, env, judge.MaxOutput)
	v.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		v.Reason = "judge harness could not answer"
		return v, err
	}
	v.Allow, v.Reason, err = judge.Decode(out)
	if err != nil {
		v.Reason = "judge returned an invalid verdict"
	}
	return v, err
}
