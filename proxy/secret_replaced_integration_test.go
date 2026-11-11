package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

// sendSwapped sends one request carrying sentinel through client and returns
// the status the sandbox saw.
func sendSwapped(ctx context.Context, t *testing.T, client *http.Client, method, url, sentinel string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// A harness that reads a 403 on a request it declared answers for its login
// as a dead login is stranded by it the same way as by a 401, so the proxy
// retries it with a fresh value (#1022): a value replaced behind the sentinel
// is what it most likely spent. The refusal is not reported, since a 403 is
// not evidence the credential is dead.
func TestHTTPProxyRetriesAForbiddenRequestTheHarnessDeclared(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const replaced = "sk-ant-oat01-REPLACEDVALUE0000000000000000000"
	const current = "sk-ant-oat01-CURRENTVALUE00000000000000000000"

	var attempts atomic.Int32
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+current {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	resolver := &rotatingResolver{
		values: []string{replaced, current},
		scope:  secrets.ReportScope{{Host: "127.0.0.1", Paths: []string{"/v1/messages"}}},
	}
	_, client := startSecretProxy(ctx, t, sentinel, resolver)

	if got := sendSwapped(ctx, t, client, http.MethodPost, origin.URL+"/v1/messages", sentinel); got != http.StatusOK {
		t.Fatalf("status = %d, want the retry's 200", got)
	}
	if n := attempts.Load(); n != 2 {
		t.Fatalf("upstream attempts = %d, want the original and one retry", n)
	}
	time.Sleep(100 * time.Millisecond)
	if reports := resolver.reports(); len(reports) != 0 {
		t.Fatalf("reports = %v, want none for a 403", resolver.outcomes())
	}
}

// A 403 anywhere a harness did not declare is what the credential may not do,
// and is passed through as it is, unretried and unreported.
func TestHTTPProxyPassesThroughAForbiddenRequestNobodyDeclared(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"

	for _, tc := range []struct {
		name  string
		scope secrets.ReportScope
		path  string
	}{
		{name: "outside the declared scope", scope: secrets.ReportScope{{Host: "127.0.0.1", Paths: []string{"/v1/messages"}}}, path: "/v1/organizations"},
		{name: "no scope declared", path: "/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(http.StatusForbidden)
			})
			defer origin.Close()

			resolver := &rotatingResolver{
				values: []string{"sk-ant-oat01-FIRSTVALUE0000000000000000000000", "sk-ant-oat01-SECONDVALUE000000000000000000000"},
				scope:  tc.scope,
			}
			_, client := startSecretProxy(ctx, t, sentinel, resolver)
			if got := sendSwapped(ctx, t, client, http.MethodPost, origin.URL+tc.path, sentinel); got != http.StatusForbidden {
				t.Fatalf("status = %d, want the upstream's 403", got)
			}
			if n := attempts.Load(); n != 1 {
				t.Fatalf("upstream attempts = %d, want no retry", n)
			}
			time.Sleep(100 * time.Millisecond)
			if reports := resolver.reports(); len(reports) != 0 {
				t.Fatalf("reports = %v, want none for a 403", resolver.outcomes())
			}
		})
	}
}

// A report names the generation of the value the upstream refused — after a
// retry, the retry's — so the control plane can tell a report about a value
// since replaced from one about the value it stores (#1022).
func TestHTTPProxyReportsTheGenerationOfTheRefusedValue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const first = "sk-ant-oat01-FIRSTVALUE0000000000000000000000"
	const second = "sk-ant-oat01-SECONDVALUE000000000000000000000"

	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer origin.Close()

	resolver := &rotatingResolver{values: []string{first, second}}
	_, client := startSecretProxy(ctx, t, sentinel, resolver)
	if got := sendSwapped(ctx, t, client, http.MethodGet, origin.URL, sentinel); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", got)
	}
	reports := waitForReports(t, &resolver.reportLog, 1)
	if reports[0].Outcome != secrets.OutcomeRejectedAfterRetry || reports[0].Generation != "gen-"+second {
		t.Fatalf("report = %+v, want %q naming the retried value's generation", reports[0], secrets.OutcomeRejectedAfterRetry)
	}
}

// The generations a pool publishes for each client reach the swapper with
// its sentinels, which is what drops a value replaced behind one.
func TestSwapperConfigCarriesEachClientsGenerations(t *testing.T) {
	cfg := swapperConfig(Config{Secrets: SecretsConfig{Clients: []SecretClient{
		{ClientID: "sandbox-1", Sentinels: []string{"S"}, Generations: map[string]string{"S": "g2"}},
		{ClientID: "sandbox-2", Sentinels: []string{"T"}},
	}}})
	if got := cfg.Generations["sandbox-1"]["S"]; got != "g2" {
		t.Fatalf("generation = %q, want g2", got)
	}
	if _, ok := cfg.Generations["sandbox-2"]; ok {
		t.Fatal("a client delivered no generations was given some")
	}
}
