package access

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/agentcreds"
)

func judgeCredentials() []agentcreds.Credential {
	return []agentcreds.Credential{{
		Name:   "github",
		EnvVar: "GITHUB_TOKEN",
		Host:   "api.github.com",
		Uses:   []agentcreds.Use{{UseID: "use_7f3c", Description: "Open a PR against the current repo"}},
	}}
}

func TestRunNeverExecutesWhenServiceRefuses(t *testing.T) {
	for _, failure := range []error{fmt.Errorf("%w: operation exceeds the approved use", agentcreds.ErrDenied), errors.New("pool judge unavailable")} {
		t.Run(failure.Error(), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			svc := &fakeService{credentials: judgeCredentials(), getErr: failure}
			serve(t, svc)
			_, stderr, code := capture(t, "", func() int { return Run([]string{"run", "--use", "use_7f3c", "--", "sh", "-c", "touch " + marker}) })
			if code == 0 {
				t.Fatal("refusal succeeded")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("refused command ran")
			}
			if !strings.Contains(stderr, strings.TrimPrefix(failure.Error(), "denied: ")) {
				t.Fatalf("missing refusal: %s", stderr)
			}
			if len(svc.gotUse.Command) == 0 || svc.gotUse.Evidence == "" {
				t.Fatal("command and evidence were not sent to the service")
			}
		})
	}
}
