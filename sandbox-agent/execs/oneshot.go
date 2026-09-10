package execs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"
)

// RunOneShot runs a bounded non-interactive child using the manager's resolved
// run identity. Cancellation kills its process group, including harness helpers.
// It has no attach surface or persisted transcript.
func (m *Manager) RunOneShot(ctx context.Context, argv []string, env map[string]string, limit int) ([]byte, error) {
	if len(argv) == 0 || limit <= 0 {
		return nil, fmt.Errorf("command and positive output limit are required")
	}
	user, err := m.ResolveUser(CreateRequest{})
	if err != nil {
		return nil, err
	}
	dir, err := m.DefaultWorkdir()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- runtime execution API; the judge supplies a fixed executable and argv shape, never caller-selected code.
	cmd.Dir = dir
	cmd.SysProcAttr, err = AgentSysProcAttr(user)
	if err != nil {
		return nil, err
	}
	effective := EnvWithRuntimeDefaults(MergeEnv(m.env, env), user)
	// Preserve the image's PATH, but explicit runtime and harness settings win.
	base := map[string]string{}
	for _, entry := range os.Environ() {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				base[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	for k, v := range effective {
		base[k] = v
	}
	keys := make([]string, 0, len(base))
	for k := range base {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+base[k])
	}
	cmd.Cancel = func() error { return killOneShot(cmd) }
	cmd.WaitDelay = time.Second
	out := &boundedOutput{limit: limit, cancel: cancel}
	// stderr is bounded too, but never exposed: it may contain harness credentials.
	diagnostic := &boundedOutput{limit: limit, cancel: cancel}
	cmd.Stdout = out
	cmd.Stderr = diagnostic
	defer func() { _ = killOneShot(cmd) }()
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("judge harness execution failed: %w", err)
	}
	if out.overflow || diagnostic.overflow {
		return nil, fmt.Errorf("harness output exceeds limit")
	}
	return out.Bytes(), nil
}

type boundedOutput struct {
	bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if n > remaining {
		b.overflow = true
		b.cancel()
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
