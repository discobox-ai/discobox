package sandboxconfig

import "testing"

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
