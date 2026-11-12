package dnsstub

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/bridge"
)

func resolverFixture(t *testing.T) *systemResolver {
	t.Helper()
	dir := t.TempDir()
	return &systemResolver{path: filepath.Join(dir, "resolv.conf"), recordPath: filepath.Join(dir, "state", "provider.json"), stub: netip.MustParseAddr("169.254.53.53")}
}
func writeResolver(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
func readResolver(t *testing.T, path string) string {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}

func TestSystemResolverPreservesProviderAcrossRestartAndRestores(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "regular", true: "symlink"}[symlink], func(t *testing.T) {
			r := resolverFixture(t)
			original := "# provider\nnameserver 10.40.0.1\nsearch example.test\noptions ndots:2 timeout:1\ndomain provider.test\n"
			target := filepath.Join(filepath.Dir(r.path), "provider.conf")
			if symlink {
				writeResolver(t, target, original)
				if err := os.Symlink("provider.conf", r.path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeResolver(t, r.path, original)
			}
			if err := r.reconcile(time.Now()); err != nil {
				t.Fatal(err)
			}
			got := readResolver(t, r.path)
			if !strings.Contains(got, "nameserver 169.254.53.53\n") || strings.Contains(got, "10.40.0.1") || !strings.Contains(got, "search example.test\noptions ndots:2 timeout:1\ndomain provider.test\n") {
				t.Fatal(got)
			}
			info, e := os.Lstat(r.path)
			if e != nil || !info.Mode().IsRegular() {
				t.Fatalf("not a regular file: %v %v", info, e)
			}
			if symlink && readResolver(t, target) != original {
				t.Fatal("provider target modified")
			}
			// A fresh process with the stub already installed keeps the original record.
			r = &systemResolver{path: r.path, recordPath: r.recordPath, stub: r.stub}
			if err := r.reconcile(time.Now()); err != nil {
				t.Fatal(err)
			}
			record, err := bridge.LoadProviderResolver(r.recordPath)
			if err != nil {
				t.Fatal(err)
			}
			if record.Content != original {
				t.Fatalf("lost original: %#v", record)
			}
			if err := r.restore(); err != nil {
				t.Fatal(err)
			}
			if readResolver(t, r.path) != original {
				t.Fatal("original not restored")
			}
			if symlink {
				link, e := os.Readlink(r.path)
				if e != nil || link != "provider.conf" {
					t.Fatalf("link %q: %v", link, e)
				}
			}
			if _, e := os.Stat(r.recordPath); !os.IsNotExist(e) {
				t.Fatalf("record remains: %v", e)
			}
		})
	}
}

func TestSystemResolverReclaimsAndBoundsProviderRewrites(t *testing.T) {
	r := resolverFixture(t)
	now := time.Now()
	for i := range 4 {
		content := "nameserver 10.40.0." + string(rune('1'+i)) + "\n"
		// Provider managers often rename a replacement inode over the file.
		replacement := r.path + ".provider"
		writeResolver(t, replacement, content)
		if err := os.Rename(replacement, r.path); err != nil {
			t.Fatal(err)
		}
		err := r.reconcile(now.Add(time.Duration(i) * time.Second))
		if i < 3 && err != nil {
			t.Fatal(err)
		}
		if i == 3 && (err == nil || readResolver(t, r.path) != content) {
			t.Fatalf("did not yield: %v", err)
		}
		record, e := bridge.LoadProviderResolver(r.recordPath)
		if e != nil || record.Content != content {
			t.Fatalf("new resolver not recorded: %#v %v", record, e)
		}
	}
}

func TestSystemResolverNeverRewritesBeforeRecordIsDurable(t *testing.T) {
	r := resolverFixture(t)
	original := "nameserver 10.40.0.1\n"
	writeResolver(t, r.path, original)
	writeResolver(t, filepath.Dir(r.recordPath), "not a directory")
	if err := r.reconcile(time.Now()); err == nil {
		t.Fatal("expected record write error")
	}
	if readResolver(t, r.path) != original {
		t.Fatal("changed resolver without saving provider")
	}
}

func TestSystemResolverDoesNotOverwriteRecordWithStub(t *testing.T) {
	r := resolverFixture(t)
	writeResolver(t, r.path, "nameserver 169.254.53.53\n")
	if err := r.reconcile(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.recordPath); !os.IsNotExist(err) {
		t.Fatalf("record created from stub: %v", err)
	}
}

func TestSystemResolverWatchesReplacement(t *testing.T) {
	r := resolverFixture(t)
	writeResolver(t, r.path, "nameserver 10.40.0.1\n")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.run(ctx, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() { cancel(); <-done })
	wait := func(want string, stub bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(r.path)
			record, _ := bridge.LoadProviderResolver(r.recordPath)
			if strings.Contains(string(data), "nameserver 169.254.53.53") == stub && record.Content == want {
				return
			}
			select {
			case <-done:
				t.Fatal("watcher stopped after yielding")
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("watcher failed to follow provider DNS")
	}
	wait("nameserver 10.40.0.1\n", true)
	for i := 2; i <= 5; i++ {
		content := fmt.Sprintf("nameserver 10.40.0.%d\n", i)
		writeResolver(t, r.path+".provider", content)
		if err := os.Rename(r.path+".provider", r.path); err != nil {
			t.Fatal(err)
		}
		wait(content, i < 4)
	}
}
