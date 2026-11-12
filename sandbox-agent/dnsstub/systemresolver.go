package dnsstub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/proxy/bridge"
	"github.com/fsnotify/fsnotify"
)

// systemResolver watches the directory rather than the inode: provider
// managers replace resolv.conf by rename. It also periodically reconciles a
// symlink's target, whose directory may differ from /etc.
type systemResolver struct {
	path, recordPath string
	stub             netip.Addr
	attempts         []time.Time
	yielded          bool
}

// RestoreSystemResolver gives back the provider's file when the pool no longer
// requests system DNS. Only the --dns instance calls it, never sibling bridges.
func RestoreSystemResolver(logger *slog.Logger) {
	r := systemResolver{path: "/etc/resolv.conf", recordPath: bridge.ProviderResolverPath}
	if err := r.restore(); err != nil {
		logger.Warn("restore provider resolver", "error", err)
	}
}

func (r *systemResolver) restore() error {
	record, err := bridge.LoadProviderResolver(r.recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Symlink != "" {
		// Recreate the link without writing through it into the provider's file.
		temp, err := os.CreateTemp(filepath.Dir(r.path), ".discobox-resolver-*")
		if err != nil {
			return err
		}
		name := temp.Name()
		if err := temp.Close(); err != nil {
			_ = os.Remove(name)
			return err
		}
		if err := os.Remove(name); err != nil {
			return err
		}
		if err := os.Symlink(record.Symlink, name); err != nil {
			return err
		}
		defer func() { _ = os.Remove(name) }()
		if err := os.Rename(name, r.path); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(r.path)); err != nil {
			return err
		}
	} else if err := replaceSynced(r.path, []byte(record.Content), 0o644); err != nil {
		return err
	}
	if err := os.Remove(r.recordPath); err != nil {
		return err
	}
	return syncDir(filepath.Dir(r.recordPath))
}

// reconcile saves the provider's new resolver before every reclaim. Three
// writes in a minute are allowed; a fourth leaves the provider's file in place
// for this process's lifetime. Write failures likewise stop reclamation.
func (r *systemResolver) reconcile(now time.Time) error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	onlyStub := false
	provider := false
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if fields[1] == r.stub.String() {
			onlyStub = true
		} else {
			provider = true
		}
	}
	if onlyStub && !provider {
		return nil
	}
	if !provider {
		return fmt.Errorf("system resolver has no provider nameserver; leaving it unchanged")
	}
	record := bridge.ProviderResolver{Content: string(data)}
	info, err := os.Lstat(r.path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		record.Symlink, err = os.Readlink(r.path)
		if err != nil {
			return err
		}
	}
	if len(record.Nameservers()) == 0 {
		return fmt.Errorf("system resolver has no usable provider nameservers")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.recordPath), 0o755); err != nil {
		return err
	}
	saved, loadErr := bridge.LoadProviderResolver(r.recordPath)
	if loadErr != nil || saved != record {
		if err := replaceSynced(r.recordPath, encoded, 0o600); err != nil {
			return fmt.Errorf("save provider resolver: %w", err)
		}
	}
	if r.yielded {
		return nil
	}
	cutoff := now.Add(-time.Minute)
	recent := r.attempts[:0]
	for _, attempt := range r.attempts {
		if attempt.After(cutoff) {
			recent = append(recent, attempt)
		}
	}
	r.attempts = recent
	if len(r.attempts) >= 3 {
		r.yielded = true
		return fmt.Errorf("provider repeatedly replaced the system resolver; leaving its file in place")
	}
	r.attempts = append(r.attempts, now)
	var out strings.Builder
	fmt.Fprintf(&out, "# Managed by discobox's DNS stub.\nnameserver %s\n", r.stub)
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && (fields[0] == "search" || fields[0] == "domain" || fields[0] == "options") {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	if err := replaceSynced(r.path, []byte(out.String()), 0o644); err != nil {
		r.yielded = true
		return err
	}
	return nil
}

func (r *systemResolver) run(ctx context.Context, logger *slog.Logger) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Warn("system DNS unchanged", "error", err)
		return
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(filepath.Dir(r.path)); err != nil {
		logger.Warn("system DNS unchanged", "error", err)
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	warned := false
	for {
		before := time.Time{}
		if len(r.attempts) > 0 {
			before = r.attempts[len(r.attempts)-1]
		}
		if err := r.reconcile(time.Now()); err != nil {
			r.yielded = true
			if !warned {
				logger.Warn("system DNS left with provider; stub still serving", "error", err)
				warned = true
			}
		}
		if !r.yielded && len(r.attempts) > 0 && !r.attempts[len(r.attempts)-1].Equal(before) {
			logger.Info("system resolver now uses sandbox DNS stub", "address", r.stub)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case _, ok := <-watcher.Events:
			if !ok {
				return
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logger.Warn("watch system resolver", "error", err)
		}
	}
}

// replaceSynced syncs both content and its rename before callers may point DNS
// at the stub. A symlink at path is replaced, never followed.
func replaceSynced(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".discobox-resolver-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
