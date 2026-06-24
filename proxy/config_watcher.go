package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ConfigWatchOptions controls file-based runtime config reload.
type ConfigWatchOptions struct {
	Path     string
	Interval time.Duration
	OnError  func(error)
}

// WatchConfigFile polls a JSON config file and applies runtime policy changes.
// The watcher never exposes a network API. Invalid config is reported and the
// previous policy remains active.
func (s *Server) WatchConfigFile(ctx context.Context, opts ConfigWatchOptions) error {
	if opts.Path == "" {
		return fmt.Errorf("config path is required")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = time.Second
	}
	var lastMod time.Time
	apply := func() {
		info, err := os.Stat(opts.Path)
		if err != nil {
			reportConfigWatchError(opts.OnError, err)
			return
		}
		if !info.ModTime().After(lastMod) {
			return
		}
		cfg, err := LoadConfigFile(opts.Path)
		if err != nil {
			reportConfigWatchError(opts.OnError, err)
			return
		}
		if err := s.ApplyConfig(cfg); err != nil {
			reportConfigWatchError(opts.OnError, err)
			return
		}
		lastMod = info.ModTime()
	}
	apply()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			apply()
		}
	}
}

// LoadConfigFile reads a JSON proxy config file.
func LoadConfigFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := DefaultConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func reportConfigWatchError(fn func(error), err error) {
	if fn != nil && err != nil {
		fn(err)
	}
}
