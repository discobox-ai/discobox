package endpoint

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDefaultSocketRejectsDarwinACLGrants(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "inherited"}[inherited], func(t *testing.T) {
			base := t.TempDir()
			if err := os.Chmod(base, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_RUNTIME_DIR", base)
			target := filepath.Join(base, "discobox")
			acl := "everyone allow search,add_file,delete_child"
			if inherited {
				target = base
				acl += ",directory_inherit"
			} else if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.CommandContext(t.Context(), "/bin/chmod", "+a", acl, target).CombinedOutput(); err != nil {
				t.Fatalf("set ACL: %v: %s", err, output)
			}
			if listener, _, cleanup, err := Listen(DefaultEndpoint()); err == nil {
				cleanup()
				listener.Close()
				t.Fatal("ACL grant accepted on default socket directory")
			}
			parsed, _ := Parse(DefaultEndpoint())
			_, client, err := HTTPClient(parsed, nil)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://discobox.local/", nil)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				t.Fatal("ACL grant accepted when dialing")
			}
		})
	}
}

func TestRuntimeDirectoryRepairDarwinACLChanged(t *testing.T) {
	_, child := repairTree(t)
	if err := os.Chmod(child, 0755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(t.Context(), "/bin/chmod", "+a", "everyone deny delete", child).CombinedOutput(); err != nil {
		t.Fatalf("set ACL: %v: %s", err, output)
	}
	plan, err := InspectRuntimeDirectories(DefaultEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if output, err := exec.CommandContext(t.Context(), "/bin/chmod", "-N", child).CombinedOutput(); err != nil {
		t.Fatalf("remove ACL: %v: %s", err, output)
	}
	if err := plan.Repair(); err == nil {
		t.Fatal("changed deny-only ACL accepted")
	}
	st, _ := os.Stat(child)
	if st.Mode().Perm() != 0755 {
		t.Fatal("changed tree was repaired")
	}
}
