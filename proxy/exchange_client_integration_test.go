package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// A client whose swaps are exchanges (a pool's own engine calling its
// driver's service, ADR 26-11-11-080 §9) has its header sentinel swapped at
// the route it is held to, as any client's is; and the answer, which can
// carry what the key obtained (an Archil process URL's token), is never
// recorded: no response body, and every response header value but the
// framing ones redacted. The request is recorded as the client sent it, with
// the sentinel. The same swap for an ordinary client keeps its answer.
func TestHTTPProxyNeverRecordsTheAnswerToAnExchangingClientsSwap(t *testing.T) {
	const sentinel = "SENTINEL0000000000000000000000000000000000"
	const realKey = "REALKEY00000000000000000000000000000000000"
	const token = "process-token-for-an-hour" //nolint:gosec // a fake token, for the test origin to hand out
	route, err := secretroute.Parse("POST api.x.test/api/**")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		exchanges bool
	}{{"exchanges", true}, {"ordinary", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			resolver := &placeResolver{grants: []secretroute.Grant{{Lists: [][]secretroute.Route{{route}}}}, value: realKey}
			origin := &routeOrigin{}
			origin.Server = newOrigin(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer key-"+realKey {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("X-Process-Url", "wss://process.x.test/?token="+token)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"success":true,"data":{"url":"wss://process.x.test/?token=`+token+`"}}`)
			})
			t.Cleanup(origin.Close)
			client, dbPath, bodyDir := startRouteProxy(ctx, t, origin, SecretClient{ClientID: "engine", Sentinels: []string{sentinel}, Exchanges: tc.exchanges}, resolver)
			port := origin.Listener.Addr().(*net.TCPAddr).Port
			target := (&url.URL{Scheme: "http", Host: net.JoinHostPort("api.x.test", strconv.Itoa(port)), Path: "/api/sandboxes/s1/connections"}).String()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer key-"+sentinel)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(got), token) {
				t.Fatalf("the call = %d %q, want 200 with the URL: the client keeps the answer", resp.StatusCode, got)
			}
			exchange := waitForHTTPExchange(t, dbPath, "client_id = ? AND url = ?", "engine", target)
			if strings.Contains(exchange.RequestHeaders, realKey) {
				t.Fatalf("the audit kept the key: %s", exchange.RequestHeaders)
			}
			if !tc.exchanges {
				if exchange.ResponseBodyFile == "" || !strings.Contains(exchange.ResponseHeaders, token) {
					t.Fatalf("an ordinary client's answer = body %q, headers %s; want it recorded", exchange.ResponseBodyFile, exchange.ResponseHeaders)
				}
				return
			}
			if exchange.ResponseBodyFile != "" || exchange.ResponseBodyFormat != "" {
				t.Fatalf("the answer to an exchanging client's swap was spooled: %q/%q under %s", exchange.ResponseBodyFormat, exchange.ResponseBodyFile, bodyDir)
			}
			if strings.Contains(exchange.ResponseHeaders, token) || !strings.Contains(exchange.ResponseHeaders, "application/json") {
				t.Fatalf("the audit kept %s, want every response header value redacted but the framing ones", exchange.ResponseHeaders)
			}
		})
	}
}
