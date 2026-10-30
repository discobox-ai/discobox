package sandboxruntime

// The sandbox agent's TLS (ADR 26-10-29-481 §3). It is the pool's setting:
// the pool tells each sandbox in its bootstrap whether its agent serves its
// API over TLS alone or plain HTTP alone, and dials it the same way, so every
// box of a pool is the same and nothing is learned from an agent before it is
// sent anything. When it serves TLS, its certificate is one this pool's mTLS
// CA signed for the sandbox's private name (sandboxconfig.AgentServerName),
// placed beside its bootstrap before its agent starts and renewed in its
// runtime-config document.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// agentTLSRecordName is the pool's record, in the sandbox's tree, of what the
// sandbox's bootstrap told its agent: a pool whose setting changes reaches the
// sandboxes it already made as they were made, until each is made again.
const agentTLSRecordName = "agent-tls.json"

type agentTLSRecord struct {
	TLS bool `json:"tls"`
}

func (r *sandboxCore) agentTLSRecordPath(sandboxID string) string {
	return filepath.Join(r.sandboxRoot(sandboxID), agentTLSRecordName)
}

// recordAgentTLS records what the sandbox's bootstrap tells its agent, as it
// is written.
func (r *sandboxCore) recordAgentTLS(sandboxID string, serves bool) error {
	data, err := json.Marshal(agentTLSRecord{TLS: serves})
	if err != nil {
		return err
	}
	path := r.agentTLSRecordPath(sandboxID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// agentServesTLS reports whether the sandbox's bootstrap told its agent to
// serve TLS. A sandbox with no record was made before the setting, with plain
// HTTP.
func (r *sandboxCore) agentServesTLS(sandboxID string) (bool, error) {
	data, err := os.ReadFile(r.agentTLSRecordPath(sandboxID))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record agentTLSRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return false, fmt.Errorf("decode %s: %w", agentTLSRecordName, err)
	}
	return record.TLS, nil
}

// agentDialer is dial as the pool reaches the sandbox's agent: over TLS,
// verified against this pool's mTLS CA for the sandbox's private name, where
// its bootstrap told it to serve TLS, and as dial is otherwise.
func (r *sandboxCore) agentDialer(sandboxID string, dial Dialer) (Dialer, error) {
	serves, err := r.agentServesTLS(sandboxID)
	if err != nil || !serves {
		return dial, err
	}
	config, err := proxyagent.SandboxAgentTLSConfig(r.root, r.projectID, r.poolID, sandboxID)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (net.Conn, error) {
		conn, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		tlsConn := tls.Client(conn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("TLS to sandbox %s's agent: %w", sandboxID, err)
		}
		return tlsConn, nil
	}, nil
}

// agentTLSFile is one file of the agent's keypair as it is placed.
type agentTLSFile struct {
	name string
	data []byte
	mode os.FileMode
}

// agentTLSFiles is the agent's keypair as it is placed: its certificate,
// public, and its key, root's alone, in the config volume's
// sandboxconfig.AgentTLSDirName.
func agentTLSFiles(keypair sandboxconfig.AgentTLS) []agentTLSFile {
	return []agentTLSFile{
		{sandboxconfig.AgentTLSCertName, []byte(keypair.Cert), 0o644},
		{sandboxconfig.AgentTLSKeyName, []byte(keypair.Key), 0o600},
	}
}

// placeAgentTLS writes the sandbox's agent keypair into a container's config
// volume on this side, configDir, before its agent starts. The volume is the
// sandbox's to write while it runs, and is reused when its container is
// replaced, so it is written through a root that refuses a link the sandbox
// planted there rather than following it out onto the host, which the pool
// agent writes as root (writeRootFile).
func placeAgentTLS(configDir string, keypair sandboxconfig.AgentTLS) error {
	root, err := os.OpenRoot(configDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Mkdir(sandboxconfig.AgentTLSDirName, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	dir, err := root.OpenRoot(sandboxconfig.AgentTLSDirName)
	if err != nil {
		return fmt.Errorf("open %s in the sandbox's config volume: %w", sandboxconfig.AgentTLSDirName, err)
	}
	defer dir.Close()
	for _, file := range agentTLSFiles(keypair) {
		if err := writeRootFile(dir, file.name, file.data, file.mode); err != nil {
			return err
		}
	}
	return nil
}

// placeAgentKeypair places the keypair a container's agent serves its API
// over TLS with, in its config volume, where its bootstrap told it to serve
// TLS: the one the pool holds now, so a renewal due while it was down is what
// it starts on.
func (r *DockerSandboxRuntime) placeAgentKeypair(sandboxID string) error {
	serves, err := r.agentServesTLS(sandboxID)
	if err != nil || !serves {
		return err
	}
	material, err := proxyagent.EnsureSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID)
	if err != nil {
		return err
	}
	if err := placeAgentTLS(r.sandboxConfigRoot(sandboxID), material.Agent); err != nil {
		return fmt.Errorf("place sandbox %s's agent keypair: %w", sandboxID, err)
	}
	return nil
}
