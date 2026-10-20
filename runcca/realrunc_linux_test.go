// runc is a Linux runtime, and the search leans on its file modes and links,
// which Windows has no counterpart of.

package runcca

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// box lays out a root with the wrapper's directory and the system's bin
// directories, the wrapper's executable linked into the first as `runc`, and
// a real runc at each of runcs. It returns the root, the PATH the wrapper's
// units give containerd and dockerd rooted there, and the wrapper's own file.
func box(t *testing.T, runcs ...string) (string, string, os.FileInfo) {
	t.Helper()
	root := t.TempDir()
	agent := filepath.Join(root, "usr/local/bin/discobox-sandbox-agent")
	write := func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(agent)
	var dirs []string
	for _, dir := range strings.Split("/opt/discobox/bin:"+systemPath, ":") {
		dirs = append(dirs, filepath.Join(root, dir))
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(agent, filepath.Join(root, "opt/discobox/bin/runc")); err != nil {
		t.Fatal(err)
	}
	for _, runc := range runcs {
		write(filepath.Join(root, runc))
	}
	self, err := os.Stat(agent)
	if err != nil {
		t.Fatal(err)
	}
	return root, strings.Join(dirs, string(filepath.ListSeparator)), self
}

func TestRealRuncIsWhereTheBoxKeepsIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		runc string
	}{
		// Docker's containerd.io, which the Debian sandbox image installs.
		{"container", "/usr/bin/runc"},
		// Ubuntu's runc package, which exe.dev's and boxd's machines carry.
		{"machine", "/usr/sbin/runc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, path, self := box(t, tc.runc)
			got, err := RealRunc(path, self)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(got, tc.runc) {
				t.Errorf("RealRunc = %s, want the box's %s", got, tc.runc)
			}
		})
	}
}

func TestRealRuncStepsOverTheWrapperHoweverItIsLinked(t *testing.T) {
	root, path, self := box(t, "/usr/bin/runc")
	dirs := filepath.SplitList(path)
	// A hard link and a link to a link of the agent, ahead of the real one.
	agent := filepath.Join(root, "usr/local/bin/discobox-sandbox-agent")
	if err := os.Link(agent, filepath.Join(dirs[1], "runc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dirs[0], "runc"), filepath.Join(dirs[2], "runc")); err != nil {
		t.Fatal(err)
	}
	got, err := RealRunc(path, self)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "/usr/bin/runc") {
		t.Errorf("RealRunc = %s, want the real /usr/bin/runc", got)
	}
}

func TestRealRuncRefusesWhenOnlyTheWrapperIsThere(t *testing.T) {
	_, path, self := box(t)
	if got, err := RealRunc(path, self); err == nil {
		t.Errorf("RealRunc = %s, want an error: only the wrapper is on PATH", got)
	}
}
