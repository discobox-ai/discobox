package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// A size takes the units docker's --memory takes, all binary, and a bare
// number stays bytes.
func TestByteSizeFlag(t *testing.T) {
	for text, want := range map[string]int64{
		"0":       0,
		"1048576": 1 << 20,
		"512m":    512 << 20,
		"8g":      8 << 30,
		"8G":      8 << 30,
		"8gb":     8 << 30,
		"8GiB":    8 << 30,
		"8 GiB":   8 << 30,
		"1.5g":    3 << 29,
		"2t":      2 << 40,
		"64k":     64 << 10,
		"100b":    100,
	} {
		var f byteSizeFlag
		if err := f.Set(text); err != nil || f.bytes != want {
			t.Errorf("Set(%q) = %d, %v; want %d", text, f.bytes, err, want)
		}
	}
	for _, text := range []string{"", "eight", "8x", "-1g", "1.5b", "8gig", "99999999t"} {
		var f byteSizeFlag
		if err := f.Set(text); err == nil {
			t.Errorf("Set(%q) = %d, want an error", text, f.bytes)
		}
	}
}

// A pool's memory is given with a unit and sent as the bytes the API takes.
func TestPoolUpdateTakesMemoryWithAUnit(t *testing.T) {
	var sent map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/pools":
			_, _ = io.WriteString(w, `{"pools":[`+poolWithResources+`]}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/projects/project-1/pools/pool-1":
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Errorf("decode body: %v", err)
			}
			_, _ = io.WriteString(w, poolWithResources)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	runCLI(t, server.URL, "admin", "pool", "update", "pool-1", "--memory", "8g", "--storage", "100GiB")
	if sent["memoryBytes"] != float64(8<<30) || sent["storageBytes"] != float64(100<<30) {
		t.Fatalf("update body = %#v, want 8 GiB of memory and 100 GiB of storage", sent)
	}
}

// A pool update says whether the pool reaches its agents over TLS only when
// the flag is given, so an update about something else leaves it alone.
func TestPoolUpdateSendsTheAgentTLSSettingOnlyWhenGiven(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want any
	}{
		{args: []string{"--memory", "8g"}},
		{args: []string{"--disable-agent-endpoint-tls"}, want: true},
		{args: []string{"--disable-agent-endpoint-tls=false"}, want: false},
	} {
		var sent map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/pools":
				_, _ = io.WriteString(w, `{"pools":[`+poolWithResources+`]}`)
			case r.Method == http.MethodPatch && r.URL.Path == "/projects/project-1/pools/pool-1":
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
					t.Errorf("decode body: %v", err)
				}
				_, _ = io.WriteString(w, poolWithResources)
			default:
				http.NotFound(w, r)
			}
		}))
		runCLI(t, server.URL, append([]string{"admin", "pool", "update", "pool-1"}, tc.args...)...)
		server.Close()
		if got, ok := sent["disableAgentEndpointTls"]; got != tc.want || ok != (tc.want != nil) {
			t.Errorf("%v: disableAgentEndpointTls = %v (sent %v), want %v", tc.args, got, ok, tc.want)
		}
	}
}

func TestFormatPoolAgentTLS(t *testing.T) {
	for _, tc := range []struct {
		pool apimodel.Pool
		want string
	}{
		{pool: apimodel.Pool{}, want: "off"},
		{pool: apimodel.Pool{DisableAgentEndpointTls: apiclientgen.NewOptBool(false)}, want: "on"},
		{pool: apimodel.Pool{DisableAgentEndpointTls: apiclientgen.NewOptBool(true)}, want: "off"},
	} {
		if got := formatPoolAgentTLS(tc.pool); got != tc.want {
			t.Errorf("formatPoolAgentTLS(%+v) = %q, want %q", tc.pool.DisableAgentEndpointTls, got, tc.want)
		}
	}
}
