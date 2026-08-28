package boot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

func readDirenvConfig(t *testing.T, home string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".config", "direnv", "direnv.toml"))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read direnv.toml: %v", err)
	}
	return string(data), true
}

func TestSeedDirenvConfigWhitelistsEverySourceTarget(t *testing.T) {
	id := selfIdentity(t)
	b := &booter{}
	sources := []sandboxconfig.Source{
		{Slug: "primary", Target: "/workspace"},
		{Slug: "docs", Target: "/home/dev/docs/"},
	}
	if err := b.seedDirenvConfig(id, sources); err != nil {
		t.Fatalf("seedDirenvConfig: %v", err)
	}
	got, ok := readDirenvConfig(t, id.home)
	if !ok {
		t.Fatal("no direnv.toml was written")
	}
	for _, want := range []string{`"/workspace"`, `"/home/dev/docs"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("direnv.toml = %q, want a whitelist entry %s", got, want)
		}
	}
	if !strings.Contains(got, "[whitelist]") {
		t.Fatalf("direnv.toml = %q, want a [whitelist] table", got)
	}
}

// A target the manifest carries twice must not be whitelisted twice, and an
// empty one must not be whitelisted at all: "" is a prefix of every path, so it
// would hand the whole filesystem to direnv.
func TestSeedDirenvConfigSkipsEmptyAndRepeatedTargets(t *testing.T) {
	id := selfIdentity(t)
	b := &booter{}
	sources := []sandboxconfig.Source{
		{Slug: "primary", Target: "/workspace"},
		{Slug: "awaiting", Target: ""},
		{Slug: "again", Target: "/workspace/"},
		{Slug: "root", Target: "/"},
	}
	if err := b.seedDirenvConfig(id, sources); err != nil {
		t.Fatalf("seedDirenvConfig: %v", err)
	}
	got, ok := readDirenvConfig(t, id.home)
	if !ok {
		t.Fatal("no direnv.toml was written")
	}
	if n := strings.Count(got, `"/workspace"`); n != 1 {
		t.Fatalf("direnv.toml = %q, want /workspace whitelisted once, got %d", got, n)
	}
	if strings.Contains(got, `""`) || strings.Contains(got, `"/"`) {
		t.Fatalf("direnv.toml = %q, want no empty or root prefix", got)
	}
}

// A sandbox with no source has nothing to trust, so it gets no config at all
// rather than one with an empty whitelist.
func TestSeedDirenvConfigWithoutSourcesWritesNothing(t *testing.T) {
	id := selfIdentity(t)
	b := &booter{}
	if err := b.seedDirenvConfig(id, nil); err != nil {
		t.Fatalf("seedDirenvConfig: %v", err)
	}
	if _, ok := readDirenvConfig(t, id.home); ok {
		t.Fatal("wrote a direnv.toml for a sandbox with no sources")
	}
	if _, err := os.Stat(filepath.Join(id.home, ".config", "direnv")); !os.IsNotExist(err) {
		t.Fatalf("stat ~/.config/direnv = %v, want it not to have been created", err)
	}
}

// The file is the user's after the first boot: direnv reads exactly one config,
// so a prefix somebody added by hand has nowhere else to live.
func TestSeedDirenvConfigLeavesAnExistingConfigAlone(t *testing.T) {
	id := selfIdentity(t)
	dir := filepath.Join(id.home, ".config", "direnv")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mine := "[whitelist]\nprefix = [ \"/home/dev/scratch\" ]\n"
	if err := os.WriteFile(filepath.Join(dir, "direnv.toml"), []byte(mine), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	b := &booter{}
	if err := b.seedDirenvConfig(id, []sandboxconfig.Source{{Slug: "primary", Target: "/workspace"}}); err != nil {
		t.Fatalf("seedDirenvConfig: %v", err)
	}
	got, ok := readDirenvConfig(t, id.home)
	if !ok {
		t.Fatal("the existing direnv.toml disappeared")
	}
	if got != mine {
		t.Fatalf("direnv.toml = %q, want it left as %q", got, mine)
	}
}
