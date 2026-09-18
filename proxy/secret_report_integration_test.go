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

// A credential the upstream refused, with nothing different to send in its
// place, is the fact ADR 0132 exists to move: the value the control plane would
// hand out now is the one that was rejected, and only the response path can
// know it.
func TestHTTPProxyReportsARejectedCredentialItCannotRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const value = "sk-ant-oat01-DEADVALUE00000000000000000000000"

	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer origin.Close()

	resolver := &settableResolver{value: value}
	client := startSecretProxy(ctx, t, sentinel, resolver)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()

	reports := waitForReports(t, &resolver.reportLog, 1)
	if len(reports) != 1 {
		t.Fatalf("reports = %v, want exactly one", reports)
	}
	got := reports[0]
	if got.Outcome != secrets.OutcomeRejected {
		t.Fatalf("outcome = %q, want %q", got.Outcome, secrets.OutcomeRejected)
	}
	if got.Sentinel != sentinel {
		t.Fatalf("sentinel = %q, want the swapped sentinel", got.Sentinel)
	}
	if got.ClientID != "sandbox-1" {
		t.Fatalf("client = %q, want the mTLS client identity", got.ClientID)
	}
	if !strings.Contains(got.Host, "127.0.0.1") {
		t.Fatalf("host = %q, want the destination the request went to", got.Host)
	}
}

// A second credential refused is what separates a dead login from a rotation
// the two ends had not converged on. The retry is the evidence, so the report
// says the retry happened.
func TestHTTPProxyReportsACredentialRejectedAfterItsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"

	var attempts atomic.Int32
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer origin.Close()

	// Two different values, both refused: the retry has something new to send
	// and it does not help.
	resolver := &rotatingResolver{values: []string{
		"sk-ant-oat01-FIRSTVALUE0000000000000000000000",
		"sk-ant-oat01-SECONDVALUE000000000000000000000",
	}}
	client := startSecretProxy(ctx, t, sentinel, resolver)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()
	drainAndClose(resp.Body)

	if n := attempts.Load(); n != 2 {
		t.Fatalf("upstream attempts = %d, want the original and one retry", n)
	}
	reports := waitForReports(t, &resolver.reportLog, 1)
	if len(reports) != 1 || reports[0].Outcome != secrets.OutcomeRejectedAfterRetry {
		t.Fatalf("reports = %v, want one %q", resolver.outcomes(), secrets.OutcomeRejectedAfterRetry)
	}
}

// The blip ADR 0059 is about reports nothing at all. A retry that worked is a
// rotation the proxy caught up with, and nobody needs to be told about a
// credential that is working.
func TestHTTPProxyReportsNothingWhenTheRetrySucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const stale = "sk-ant-oat01-STALEVALUE0000000000000000000000"
	const rotated = "sk-ant-oat01-ROTATEDVALUE00000000000000000000"

	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+rotated {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	resolver := &rotatingResolver{values: []string{stale, rotated}}
	client := startSecretProxy(ctx, t, sentinel, resolver)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()
	drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the retry's 200", resp.StatusCode)
	}
	if got := resolver.outcomes(); len(got) != 0 {
		t.Fatalf("reports = %v, want none: the retry fixed it", got)
	}
}

// A harness that has decided it is logged out retries hard, and every one of
// those retries is the same fact. The control plane hears it once, which also
// bounds the forced refresh it performs on the other end.
func TestHTTPProxyReportsARejectedCredentialOncePerCooldown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"

	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer origin.Close()

	resolver := &settableResolver{value: "sk-ant-oat01-DEADVALUE00000000000000000000000"}
	client := startSecretProxy(ctx, t, sentinel, resolver)

	for range 5 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+sentinel)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		defer resp.Body.Close()
		drainAndClose(resp.Body)
	}

	reports := waitForReports(t, &resolver.reportLog, 1)
	if len(reports) != 1 {
		t.Fatalf("reports = %v, want one for five identical rejections", resolver.outcomes())
	}
}

// A credential that starts working again clears what was said about it, however
// it came to be fixed — so the window stops asking for a sign-in that has
// already happened.
func TestHTTPProxyReportsAcceptanceAfterARejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const dead = "sk-ant-oat01-DEADVALUE00000000000000000000000"
	const good = "sk-ant-oat01-GOODVALUE00000000000000000000000"

	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+good {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	resolver := &settableResolver{value: dead}
	client := startSecretProxy(ctx, t, sentinel, resolver, func(cfg *Config) {
		// No positive caching, so the second request resolves again and picks
		// up the replaced credential the way a re-configured harness would.
		cfg.Secrets.PositiveTTLSeconds = 0
	})

	send := func() int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+sentinel)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		defer resp.Body.Close()
		drainAndClose(resp.Body)
		return resp.StatusCode
	}

	if got := send(); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream's 401", got)
	}
	waitForReports(t, &resolver.reportLog, 1)

	// The credential is replaced out here, the way a reconfigure replaces it.
	resolver.set(good)
	if got := send(); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 on the replaced credential", got)
	}

	reports := waitForReports(t, &resolver.reportLog, 2)
	if len(reports) != 2 {
		t.Fatalf("reports = %v, want the rejection and its clearance", resolver.outcomes())
	}
	if reports[0].Outcome != secrets.OutcomeRejected || reports[1].Outcome != secrets.OutcomeAccepted {
		t.Fatalf("outcomes = %v, want rejected then accepted", resolver.outcomes())
	}
}

// A working credential says nothing. The clearance exists to retract a
// rejection, and a proxy that reported every successful swap would be a second
// audit trail nobody asked for — over the control plane's network, at request
// rate.
func TestHTTPProxyReportsNothingForACredentialThatWorks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const value = "sk-ant-oat01-GOODVALUE00000000000000000000000"

	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	resolver := &settableResolver{value: value}
	client := startSecretProxy(ctx, t, sentinel, resolver)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()
	drainAndClose(resp.Body)

	if got := resolver.outcomes(); len(got) != 0 {
		t.Fatalf("reports = %v, want none", got)
	}
}

// A 401 on a request this proxy put no credential into belongs to whatever the
// sandbox sent, and saying otherwise would condemn a credential on the strength
// of somebody else's failed request.
func TestHTTPProxyReportsNothingForAnUnswappedRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"

	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer origin.Close()

	resolver := &settableResolver{value: "sk-ant-oat01-GOODVALUE00000000000000000000000"}
	client := startSecretProxy(ctx, t, sentinel, resolver)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer not-a-sentinel")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()
	drainAndClose(resp.Body)

	if got := resolver.outcomes(); len(got) != 0 {
		t.Fatalf("reports = %v, want none for a credential the sandbox brought itself", got)
	}
}

// An expired session is commonly answered with a redirect to a sign-in page.
// Taking that as "the credential works again" would retract a standing
// rejection using the upstream's own way of saying the credential is dead.
func TestHTTPProxyDoesNotTreatARedirectAsAcceptance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const dead = "sk-ant-oat01-DEADVALUE00000000000000000000000"

	var rejectOnce atomic.Bool
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		if !rejectOnce.Swap(true) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// The way a lot of upstreams say "you are not signed in".
		w.Header().Set("Location", "https://example.com/login")
		w.WriteHeader(http.StatusFound)
	})
	defer origin.Close()

	resolver := &settableResolver{value: dead}
	client := startSecretProxy(ctx, t, sentinel, resolver, func(cfg *Config) {
		cfg.Secrets.PositiveTTLSeconds = 0
	})
	// The redirect is the answer, not a hop to follow.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	send := func() {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+sentinel)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		defer resp.Body.Close()
		drainAndClose(resp.Body)
	}

	send()
	waitForReports(t, &resolver.reportLog, 1)
	send()

	// Give an acceptance every chance to turn up before concluding it did not.
	time.Sleep(200 * time.Millisecond)
	for _, report := range resolver.reports() {
		if report.Outcome == secrets.OutcomeAccepted {
			t.Fatalf("reports = %v, want no clearance from a redirect", resolver.outcomes())
		}
	}
}
