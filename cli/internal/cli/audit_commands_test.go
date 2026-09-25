package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	idpkg "github.com/discobox-ai/x/id"
)

func TestAuditHTTPSendsStatusAndBlocked(t *testing.T) {
	query, _, _, err := runAuditHTTP(t, `{"exchanges":[],"unavailablePools":[]}`, "--status", "5xx", "--blocked")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if query.Get("minStatus") != "500" || query.Get("maxStatus") != "599" || query.Get("blocked") != "true" || query.Has("order") {
		t.Fatalf("query = %v", query)
	}
}

// runAudit runs an `admin audit` subcommand against handler for ctx's life.
func runAudit(ctx context.Context, t *testing.T, handler http.HandlerFunc, args ...string) (string, string, error) {
	t.Helper()
	server := httptest.NewServer(ignoringPortProbe(handler))
	defer server.Close()
	stdout, stderr := new(strings.Builder), new(strings.Builder)
	cmd := NewRootCommand()
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "admin", "audit"}, args...))
	err := cmd.ExecuteContext(ctx)
	return stdout.String(), stderr.String(), err
}

// A follower reads the backlog newest first once, then forward from the newest
// record it printed less the lookback, and prints each verdict once.
func TestAuditCredsFollowReadsForwardFromTheLastVerdict(t *testing.T) {
	interval := auditFollowInterval
	auditFollowInterval = 5 * time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var mu sync.Mutex
	var forwardSince []string
	verdict := func(id, useID, at string) string {
		return `{"id":"` + id + `","projectId":"project-1","sandboxId":"sbx_1","useId":"` + useID + `","allow":true,"volunteered":false,"createdAt":"` + at + `","command":["gh"]}`
	}
	stdout, _, err := runAudit(ctx, t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Query().Get("order") != "asc" {
			_, _ = w.Write([]byte(`{"credentialVerdicts":[` + verdict("cv_2", "use_two", "2026-09-17T10:02:00Z") + `,` + verdict("cv_1", "use_one", "2026-09-17T10:01:00Z") + `]}`))
			return
		}
		forwardSince = append(forwardSince, r.URL.Query().Get("since"))
		// The newest printed verdict comes back, as an inclusive bound
		// returns it, beside one that is new.
		_, _ = w.Write([]byte(`{"credentialVerdicts":[` + verdict("cv_2", "use_two", "2026-09-17T10:02:00Z") + `,` + verdict("cv_3", "use_three", "2026-09-17T10:03:00Z") + `]}`))
		if len(forwardSince) == 3 {
			cancel()
		}
	}, "creds", "--follow")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	one, two, three := strings.Index(stdout, "use_one"), strings.Index(stdout, "use_two"), strings.Index(stdout, "use_three")
	if one < 0 || two < one || three < two || strings.Count(stdout, "use_two") != 1 || strings.Count(stdout, "use_three") != 1 {
		t.Fatalf("stdout = %q, want each verdict once, oldest first", stdout)
	}
	mu.Lock()
	defer mu.Unlock()
	since, err := time.Parse(time.RFC3339, forwardSince[0])
	if err != nil || !since.Equal(time.Date(2026, 9, 17, 10, 2, 0, 0, time.UTC).Add(-auditWriterLookback)) {
		t.Fatalf("first forward read since %q, want the newest printed verdict less the lookback", forwardSince[0])
	}
}

func TestAuditHTTPBodyReadsTheRecordingFromItsPool(t *testing.T) {
	poolID, err := idpkg.New("pool")
	if err != nil {
		t.Fatal(err)
	}
	recorded := "recorded " + string(rune(0x1b)) + "[2J bytes"
	var gotPath, gotSandbox string
	stdout, stderr, err := runAudit(context.Background(), t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotSandbox = r.URL.Path, r.URL.Query().Get("sandboxId")
		w.Header().Set(httpAuditFormatHeader, "framed")
		_, _ = w.Write([]byte(recorded))
	}, "http", "--pool", poolID, "--body", "http_42", "--part", "stream")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotPath != "/api/projects/project-1/pools/"+poolID+"/audit/http/http_42/stream" || gotSandbox != "" {
		t.Fatalf("read %s?sandboxId=%s", gotPath, gotSandbox)
	}
	// Not a terminal: the recording is written exactly, for a redirect to a
	// file to keep.
	if stdout != recorded || !strings.Contains(stderr, "framed") {
		t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
	}

	for _, args := range [][]string{
		// A bare row number is the audit database's spelling, not the API's.
		{"http", "--pool", poolID, "--body", "42"},
		{"http", "--body", "http_42"},
		{"http", "--pool", poolID, "--body", "http_42", "--follow"},
		{"http", "--pool", poolID, "--body", "http_42", "--part", "headers"},
		{"http", "--part", "request"},
		{"http", "--status", "teapot"},
	} {
		if _, _, err := runAudit(context.Background(), t, func(_ http.ResponseWriter, r *http.Request) {
			t.Errorf("%v reached the server: %s", args, r.URL)
		}, args...); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
}

func TestAuditHooksAndExecsSendTheirFiltersAndPrintNewestFirst(t *testing.T) {
	sandboxID, err := idpkg.New("sbx")
	if err != nil {
		t.Fatal(err)
	}
	var query url.Values
	stdout, _, err := runAudit(context.Background(), t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/project-1/sandboxes/"+sandboxID+"/harness-hooks" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		// The agent answers the latest hooks oldest first.
		_, _ = w.Write([]byte(`{"hooks":[
			{"id":"evt_hook1","provider":"claude-code","event":"SessionStart","payload":{},"createdAt":"2026-09-17T10:00:00Z"},
			{"id":"h2","provider":"claude-code","event":"PreToolUse","payload":{},"createdAt":"2026-09-17T10:01:00Z"}]}`))
	}, "hooks", "--discobox-id", sandboxID, "--provider", "claude-code", "--event", "PreToolUse", "--since", "1h")
	if err != nil {
		t.Fatalf("hooks: %v", err)
	}
	if query.Get("provider") != "claude-code" || query.Get("event") != "PreToolUse" || query.Get("since") == "" || query.Has("order") {
		t.Fatalf("hooks query = %v", query)
	}
	if strings.Index(stdout, "PreToolUse") > strings.Index(stdout, "SessionStart") {
		t.Fatalf("hooks not newest first:\n%s", stdout)
	}

	stdout, _, err = runAudit(context.Background(), t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/project-1/sandboxes/"+sandboxID+"/exec-events" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[{"id":"e1","execId":"exec_1","type":"exec.start.failed","message":"no such file\u001b[2J","details":{},"createdAt":"2026-09-17T10:00:00Z"}]}`))
	}, "execs", "--discobox-id", sandboxID, "--type", "exec.start.failed")
	if err != nil {
		t.Fatalf("execs: %v", err)
	}
	if query.Get("type") != "exec.start.failed" || !strings.Contains(stdout, "exec.start.failed") || strings.ContainsRune(stdout, 0x1b) {
		t.Fatalf("execs query = %v, stdout = %q", query, stdout)
	}

	if _, _, err := runAudit(context.Background(), t, func(http.ResponseWriter, *http.Request) {}, "hooks"); err == nil {
		t.Fatal("hooks without --discobox-id was accepted")
	}
}

// The timeline labels every record with who attests it, names a trail it could
// not read, and never lets --trusted include what a discobox wrote.
func TestAuditListMergesTrailsAndLabelsTheirAttestors(t *testing.T) {
	sandboxID, err := idpkg.New("sbx")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	asked := map[string]bool{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/audit/http"):
			asked["http"] = true
			_, _ = w.Write([]byte(`{"exchanges":[{"poolId":"pool-a","id":"http_7","createdAt":"2026-09-17T10:03:00Z","sandboxId":"` + sandboxID + `","method":"GET","url":"https://api.github.com/","host":"api.github.com","status":200,"blocked":false,"swappedUseIds":["use_1"]}],"unavailablePools":[]}`))
		case strings.HasSuffix(r.URL.Path, "/audit/dns"):
			asked["dns"] = true
			_, _ = w.Write([]byte(`{"queries":[{"poolId":"pool-a","id":"dns_3","createdAt":"2026-09-17T10:00:00Z","sandboxId":"` + sandboxID + `","name":"api.github.com","type":"A","rcode":"NOERROR","answers":["192.0.2.1"]}],"unavailablePools":[]}`))
		case strings.HasSuffix(r.URL.Path, "/credential-verdicts"):
			asked["creds"] = true
			_, _ = w.Write([]byte(`{"credentialVerdicts":[
				{"id":"cvd_1","projectId":"project-1","sandboxId":"` + sandboxID + `","useId":"use_1","allow":true,"volunteered":false,"createdAt":"2026-09-17T10:02:00Z","command":["gh"]},
				{"id":"cvd_2","projectId":"project-1","sandboxId":"` + sandboxID + `","useId":"use_2","allow":false,"volunteered":true,"createdAt":"2026-09-17T10:01:00Z","command":["curl"]}]}`))
		case strings.HasSuffix(r.URL.Path, "/harness-hooks"):
			asked["hooks"] = true
			_, _ = w.Write([]byte(`{"hooks":[{"id":"evt_hook1","provider":"claude-code","event":"PreToolUse","payload":{},"createdAt":"2026-09-17T10:04:00Z"}]}`))
		case strings.HasSuffix(r.URL.Path, "/exec-events"):
			asked["execs"] = true
			// What the pool agent answers for a stopped discobox, relayed.
			http.Error(w, "discobox is stopped: reading it does not start it", http.StatusConflict)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}

	stdout, stderr, err := runAudit(context.Background(), t, handler, "list", "--discobox-id", sandboxID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 5 {
		t.Fatalf("stdout = %q, want a header and four records", stdout)
	}
	if asked["dns"] {
		t.Fatal("list read the DNS trail without --source naming it")
	}
	if got := strings.Fields(lines[0]); !slices.Equal(got, []string{"TIME", "SOURCE", "ID", "RECORD"}) {
		t.Fatalf("header = %v, want time, source, the ID and the record", got)
	}
	// Every ID is shown in the one spelling `audit get` takes, the pool trail's
	// row number included.
	for i, want := range [][]string{
		{"hooks", "evt_hook1", "PreToolUse"},
		{"http", "http_7", "uses=use_1"},
		{"creds", "cvd_1", "allow use_1 (use)"},
		{"creds", "cvd_2", "deny use_2 (report)"},
	} {
		for _, field := range want {
			if !strings.Contains(lines[i+1], field) {
				t.Fatalf("record %d = %q, want %q; stdout:\n%s", i, lines[i+1], field, stdout)
			}
		}
	}
	// Neither the discobox nor its pool is named on a row: every record in the
	// listing is that one discobox's, and it runs on one pool.
	for _, absent := range []string{sandboxID, "pool-a"} {
		for _, line := range lines[1:] {
			if strings.Contains(line, absent) {
				t.Fatalf("record %q names %q, which is the same for every row", line, absent)
			}
		}
	}
	if !strings.Contains(stderr, "execs could not be read") || !strings.Contains(stderr, "stopped") {
		t.Fatalf("stderr = %q, want the stopped discobox's exec trail named as missing", stderr)
	}

	// Named, the DNS trail joins the timeline under the pool's attestation.
	stdout, _, err = runAudit(context.Background(), t, handler, "list", "--discobox-id", sandboxID, "--source", "dns")
	if err != nil {
		t.Fatalf("list --source dns: %v", err)
	}
	if !strings.Contains(stdout, "dns_3") || !strings.Contains(stdout, "A api.github.com NOERROR 192.0.2.1") {
		t.Fatalf("--source dns = %q, want the lookup", stdout)
	}

	// The attestor is gone from the table, not from the record: -o json keeps
	// it, which is what a reader deciding whether a record is evidence needs
	// (ADR 0130 §2).
	mu.Lock()
	asked = map[string]bool{}
	mu.Unlock()
	stdout, _, err = runAudit(context.Background(), t, handler, "list", "--discobox-id", sandboxID, "--source", "http,creds", "-o", "json")
	if err != nil {
		t.Fatalf("list -o json: %v", err)
	}
	if asked["hooks"] || asked["execs"] || asked["dns"] {
		t.Fatalf("--source http,creds read other trails: %v", asked)
	}
	var got struct {
		Records []struct {
			Source   string `json:"source"`
			Attestor string `json:"attestor"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if len(got.Records) != 3 {
		t.Fatalf("records = %+v, want the request and both verdicts", got.Records)
	}
	attestors := map[string]string{}
	for _, record := range got.Records {
		attestors[record.Attestor] = record.Source
	}
	if attestors["pool"] != "http" || attestors["control-plane"] != "creds" || attestors["sandbox"] != "creds" {
		t.Fatalf("attestors = %v, want the pool's request and both kinds of verdict", attestors)
	}

	if _, _, err := runAudit(context.Background(), t, handler, "list"); err == nil {
		t.Fatal("list without --discobox-id was accepted")
	}
}

// The consolidated follow: one timeline over every trail, each read forward
// from the merge's own cursor, printing each record once as it is recorded.
func TestAuditListFollowMergesEveryTrailAsItIsRecorded(t *testing.T) {
	sandboxID, err := idpkg.New("sbx")
	if err != nil {
		t.Fatal(err)
	}
	interval := auditFollowInterval
	auditFollowInterval = 5 * time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	at := func(second int) string {
		return time.Date(2026, 9, 17, 10, 0, second, 0, time.UTC).Format(time.RFC3339)
	}
	exchange := func(id int, second int) string {
		return `{"poolId":"pool-a","id":"http_` + strconv.Itoa(id) + `","createdAt":"` + at(second) + `","sandboxId":"` + sandboxID +
			`","method":"GET","url":"https://api.github.com/","host":"api.github.com","status":200,"blocked":false,"swappedUseIds":[]}`
	}
	verdict := func(id, useID string, second int) string {
		return `{"id":"` + id + `","projectId":"project-1","sandboxId":"` + sandboxID + `","useId":"` + useID +
			`","allow":true,"volunteered":false,"createdAt":"` + at(second) + `","command":["gh"]}`
	}
	hook := func(id, event string, second int) string {
		return `{"id":"` + id + `","provider":"claude-code","event":"` + event + `","payload":{},"createdAt":"` + at(second) + `"}`
	}

	var mu sync.Mutex
	// round counts the forward reads of the pool trail, which paces the test:
	// each trail gains a record between rounds.
	round := 0
	forward := map[string]int{}
	var poolCursors []string
	stdout, _, err := runAudit(ctx, t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		asc := r.URL.Query().Get("order") == "asc"
		switch {
		case strings.HasSuffix(r.URL.Path, "/audit/http"):
			if asc {
				forward["http"]++
				round++
				poolCursors = append(poolCursors, strings.Join(r.URL.Query()["after"], ","))
			}
			rows := []string{exchange(1, 1)}
			if round >= 2 {
				rows = append(rows, exchange(2, 3))
			}
			_, _ = w.Write([]byte(`{"exchanges":[` + strings.Join(rows, ",") + `],"unavailablePools":[]}`))
		case strings.HasSuffix(r.URL.Path, "/audit/dns"):
			if asc {
				forward["dns"]++
			}
			_, _ = w.Write([]byte(`{"queries":[],"unavailablePools":[]}`))
		case strings.HasSuffix(r.URL.Path, "/credential-verdicts"):
			if asc {
				forward["creds"]++
			}
			rows := []string{verdict("cv_1", "use_one", 2)}
			if round >= 3 {
				rows = append(rows, verdict("cv_2", "use_two", 4))
			}
			_, _ = w.Write([]byte(`{"credentialVerdicts":[` + strings.Join(rows, ",") + `]}`))
		case strings.HasSuffix(r.URL.Path, "/harness-hooks"):
			if asc {
				forward["hooks"]++
			}
			rows := []string{hook("h1", "SessionStart", 1)}
			if round >= 4 {
				rows = append(rows, hook("h2", "PreToolUse", 5))
			}
			_, _ = w.Write([]byte(`{"hooks":[` + strings.Join(rows, ",") + `]}`))
		case strings.HasSuffix(r.URL.Path, "/exec-events"):
			if asc {
				forward["execs"]++
			}
			_, _ = w.Write([]byte(`{"events":[{"id":"e1","execId":"exec_1","type":"exec.started","details":{},"createdAt":"` + at(1) + `"}]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if round >= 5 {
			cancel()
		}
	}, "list", "--discobox-id", sandboxID, "--follow")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if forward["dns"] != 0 {
		t.Fatal("follow read the DNS trail without --source naming it")
	}
	for _, trail := range []string{"http", "creds", "hooks", "execs"} {
		if forward[trail] == 0 {
			t.Fatalf("%s was never read forward: %v", trail, forward)
		}
	}
	// The pool trail is followed by cursor, one per pool, from the first poll
	// after a row of that pool has been printed.
	if poolCursors[0] != "pool-a:http_1" {
		t.Fatalf("first forward read of the pool trail asked after %q, want pool-a:http_1", poolCursors[0])
	}
	if last := poolCursors[len(poolCursors)-1]; last != "pool-a:http_2" {
		t.Fatalf("last forward read asked after %q, want the newest row printed", last)
	}
	// The backlog, oldest first, then each new record once, in the order it
	// was recorded — across trails.
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		for _, want := range []string{"exec.started", "SessionStart", "use_one", "GET", "PreToolUse", "use_two"} {
			if strings.Contains(line, want) {
				got = append(got, want)
			}
		}
	}
	if len(got) != 7 {
		t.Fatalf("printed %v, want the four backlog records and the three that arrived:\n%s", got, stdout)
	}
	// Both records at 10:00:01 and the verdict at :02 are the backlog; the
	// pool's :03, the verdict at :04 and the hook at :05 arrived while
	// following, and each is printed once, in time order.
	if tail := got[4:]; !slices.Equal(tail, []string{"GET", "use_two", "PreToolUse"}) {
		t.Fatalf("records that arrived while following = %v, want them in the order recorded:\n%s", tail, stdout)
	}
}

// -o json reads back past one page as the table does: no request asks for more
// than an endpoint answers, every exchange arrives once, and a pool that every
// page names as unavailable is named once.
func TestAuditHTTPJSONPagesBackPastOnePage(t *testing.T) {
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	const total = 2500
	exchange := func(i int) map[string]any {
		return map[string]any{
			"poolId": "pool-a", "id": "http_" + strconv.Itoa(i+1), "sandboxId": "sbx_1", "method": "GET",
			"createdAt": base.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano),
			"url":       "https://example.com/", "host": "example.com", "status": 200, "blocked": false, "swappedUseIds": []string{},
		}
	}
	var mu sync.Mutex
	var limits []int
	stdout, _, err := runAudit(context.Background(), t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		limit, _ := strconv.Atoi(query.Get("limit"))
		mu.Lock()
		limits = append(limits, limit)
		mu.Unlock()
		if limit > auditPageLimit {
			http.Error(w, "limit is past the maximum", http.StatusBadRequest)
			return
		}
		newest := total - 1
		if raw := query.Get("until"); raw != "" {
			until, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				t.Errorf("until %q: %v", raw, err)
			}
			newest = int(until.Sub(base) / time.Millisecond)
		}
		var page []map[string]any
		for i := newest; i >= 0 && len(page) < limit; i-- {
			page = append(page, exchange(i))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"exchanges":        page,
			"unavailablePools": []map[string]string{{"poolId": "pool-b", "reason": "it did not answer"}},
		})
	}, "http", "--limit", strconv.Itoa(total), "-o", "json")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var body struct {
		Exchanges        []struct{ ID string }
		UnavailablePools []struct{ PoolID string }
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Exchanges) != total || body.Exchanges[0].ID != "http_"+strconv.Itoa(total) || body.Exchanges[total-1].ID != "http_1" {
		t.Fatalf("read %d exchanges, want all %d newest first", len(body.Exchanges), total)
	}
	if len(body.UnavailablePools) != 1 || body.UnavailablePools[0].PoolID != "pool-b" {
		t.Fatalf("unavailable = %+v, want pool-b once", body.UnavailablePools)
	}
	if len(limits) < 3 {
		t.Fatalf("made %d requests, want a page per thousand", len(limits))
	}
}

// A follower pages forward by since, from the last record of a full page, so
// since goes out to the nanosecond: truncated to the second, a second holding
// more than a page would answer the same page forever.
func TestAuditSinceKeepsFractionalSeconds(t *testing.T) {
	query, _, _, err := runAuditHTTP(t, `{"exchanges":[],"unavailablePools":[]}`, "--since", "2026-09-17T10:00:00.123456789Z")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := query.Get("since"); got != "2026-09-17T10:00:00.123456789Z" {
		t.Fatalf("since = %q, want it to the nanosecond", got)
	}
}
