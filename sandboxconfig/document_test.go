package sandboxconfig

import (
	"testing"

	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/sandboxpath"
)

// Boot materialized the image label's groups into /etc/group while the exec
// defaults preferred the manifest user's. A sandbox declaring its own groups
// therefore had them in every exec's credential while the OS account was never
// added to them. One function, one answer.
func TestSandboxGroupsHasOneAuthoritativeAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "the image label alone",
			cfg:  Config{AdditionalGroups: []string{"docker"}},
			want: []string{"docker"},
		},
		{
			// All-or-nothing: naming any replaces the label's rather than
			// adding to them, so a caller can run with fewer.
			name: "the manifest user replaces the label",
			cfg: Config{
				AdditionalGroups: []string{"docker", "video"},
				User:             User{AdditionalGroups: []string{"audio"}},
			},
			want: []string{"audio"},
		},
		{
			name: "naming none inherits the label",
			cfg: Config{
				AdditionalGroups: []string{"docker"},
				User:             User{Name: "dev"},
			},
			want: []string{"docker"},
		},
		{name: "neither", cfg: Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.SandboxGroups()
			if len(got) != len(tc.want) {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("groups = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Boot creates the working root and chowns it, and the agent starts execs
// there. They have to name the same directory: a manifest written by a pool
// agent too old to state one must not send the agent somewhere boot never
// touched.
// The default is the sandbox platform's.
func TestWorkingRootFallsBackToThePlatformDefault(t *testing.T) {
	linux := sandboxpath.For(platform.Platform{OS: "linux", Arch: "amd64"})
	windows := sandboxpath.For(platform.Platform{OS: "windows", Arch: "amd64"})
	for _, tc := range []struct {
		name  string
		cfg   Config
		paths sandboxpath.Paths
		want  string
	}{
		{
			name:  "stated",
			cfg:   Config{AgentRuntime: AgentRuntime{WorkingRoot: "/srv/work"}},
			paths: linux,
			want:  "/srv/work",
		},
		{
			name:  "not stated",
			paths: linux,
			want:  "/workspace",
		},
		{
			name:  "whitespace is not a statement",
			cfg:   Config{AgentRuntime: AgentRuntime{WorkingRoot: "  "}},
			paths: linux,
			want:  "/workspace",
		},
		{
			name:  "not stated on windows",
			paths: windows,
			want:  `C:\workspace`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.WorkingRoot(tc.paths); got != tc.want {
				t.Fatalf("WorkingRoot() = %q, want %q", got, tc.want)
			}
		})
	}
}
