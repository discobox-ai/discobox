package proxyagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/judge"
	"github.com/discobox-ai/discobox/proxy"
)

const (
	zeroSHA = "0000000000000000000000000000000000000000"
	oldSHA  = "1111111111111111111111111111111111111111"
	newSHA  = "eff2d8a0000000000000000000000000000000aa"
)

// pkt is one pkt-line.
func pkt(payload string) string { return fmt.Sprintf("%04x%s", len(payload)+4, payload) }

// pushBody is a receive-pack request: the ref updates, the first carrying the
// capabilities, a flush, and a packfile header claiming objects.
func pushBody(caps string, commands []string, objects int) []byte {
	var body strings.Builder
	for i, command := range commands {
		if i == 0 {
			command += "\x00" + caps
		}
		body.WriteString(pkt(command + "\n"))
	}
	body.WriteString("0000")
	if objects > 0 {
		body.WriteString("PACK\x00\x00\x00\x02")
		body.WriteString(string([]byte{0, 0, 0, byte(objects)}))
		body.WriteString("compressed objects follow")
	}
	return []byte(body.String())
}

// pushRequest is a push to github.com carrying the use's sentinel, as git
// sends one.
func pushRequest(body []byte) proxy.SecretAuthorizeRequest {
	header := http.Header{}
	header.Set("Authorization", "Basic "+testEphemeral)
	header.Set("Content-Type", "application/x-git-receive-pack-request")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set("User-Agent", "git/2.54.0")
	return proxy.SecretAuthorizeRequest{
		ClientID:  "sandbox-1",
		Sentinels: []string{testEphemeral},
		Method:    http.MethodPost,
		Host:      "github.com",
		URL:       "https://github.com:443/discobox-ai/ultraviolet.git/git-receive-pack",
		Header:    header,
		Body:      proxy.NewSecretRequestBody(io.NopCloser(bytes.NewReader(body))),
	}
}

// onGitHub moves the test pool's use to github.com, where a push goes.
func onGitHub(resolver *secretResolver) {
	live := resolver.activations.byEphemeral[testEphemeral]
	live.Hosts = []string{"github.com"}
	resolver.activations.byEphemeral[testEphemeral] = live
}

// A push is described by what it does from the first ask: which refs it
// changes and how. That is the whole of what a push does, and before this the
// judge was told only that its body "is not text".
func TestAGitPushIsDescribedByTheRefsItChanges(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": true, "reason": "a new branch on the approved fork"}, http.StatusOK)
	onGitHub(resolver)
	body := pushBody("report-status side-band-64k agent=git/2.54.0", []string{
		zeroSHA + " " + newSHA + " refs/heads/discobox-link-reset",
		oldSHA + " " + newSHA + " refs/heads/main",
		oldSHA + " " + zeroSHA + " refs/heads/old",
	}, 3)

	verdict, err := resolver.Authorize(context.Background(), pushRequest(body))
	if err != nil || !verdict.Allow {
		t.Fatalf("Authorize() = %+v, %v; want the first round's allow", verdict, err)
	}
	asks := asked()
	if len(asks) != 1 {
		t.Fatalf("asked %d times, want a push decided on its first ask", len(asks))
	}
	request := asks[0].Request
	if request.Protocol == nil || request.Protocol.Name != judge.ProtocolGitReceivePack || request.Endpoint != nil {
		t.Fatalf("recognized as %+v / %+v, want a git push and no endpoint", request.Protocol, request.Endpoint)
	}
	if request.Body.Parser == nil || request.Body.Parser.Name != judge.ParserGitReceivePack || request.Body.Supplied() {
		t.Fatalf("body = %+v, want it described by the git parser and not shown", request.Body)
	}
	var metadata struct {
		Commands     []map[string]string `json:"commands"`
		Capabilities []string            `json:"capabilities"`
		Packfile     map[string]int      `json:"packfile"`
	}
	if err := json.Unmarshal(request.Body.Metadata, &metadata); err != nil {
		t.Fatalf("metadata %s: %v", request.Body.Metadata, err)
	}
	want := []map[string]string{
		{"op": "create", "ref": "refs/heads/discobox-link-reset", "new": "eff2d8a00000"},
		{"op": "update", "ref": "refs/heads/main", "old": "111111111111", "new": "eff2d8a00000"},
		{"op": "delete", "ref": "refs/heads/old", "old": "111111111111"},
	}
	if fmt.Sprint(metadata.Commands) != fmt.Sprint(want) {
		t.Fatalf("commands = %v, want %v", metadata.Commands, want)
	}
	if len(metadata.Capabilities) != 3 || metadata.Packfile["objects"] != 3 || metadata.Packfile["bytes"] != 37 {
		t.Fatalf("metadata = %s, want the capabilities and the packfile's size and objects", request.Body.Metadata)
	}
	if strings.Contains(string(request.Body.Metadata), testEphemeral) {
		t.Fatal("the metadata carries a sentinel")
	}
	// The control plane's own check, and the guidance it would add.
	job := judge.Job{Kind: judge.KindRequest, Purpose: "p", Host: "github.com", Round: 1, Request: request}
	if err := job.Validate(); err != nil {
		t.Fatalf("the first ask is not a job the control plane would take: %v", err)
	}
	if len(judge.GuidanceFor(request)) == 0 {
		t.Fatal("a recognized push brings no guidance")
	}
}

// Asked for, a push is shown as its ref updates, one to a line, and not as
// the binary packfile after them.
func TestAPushShownIsItsRefUpdates(t *testing.T) {
	resolver, asked := judgingPoolFunc(t, needsThenDecides())
	onGitHub(resolver)
	body := pushBody("report-status", []string{zeroSHA + " " + newSHA + " refs/heads/topic"}, 1)
	if _, err := resolver.Authorize(context.Background(), pushRequest(body)); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	shown := asked()[1].Request.Body
	// Everything before the packfile, whole: the packfile is the metadata's
	// to describe, and is not missing from what was asked for.
	if shown.Content == nil || *shown.Content != zeroSHA+" "+newSHA+" refs/heads/topic report-status\n" || shown.Missing != "" {
		t.Fatalf("shown = %+v, want the ref update as sent", shown)
	}
}

// A request that claims to be a push and does not read as one is shown as
// that, which counts against it, rather than as some text.
func TestABodyThatIsNotTheProtocolItClaimsSaysSo(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": false, "reason": "not a push"}, http.StatusOK)
	onGitHub(resolver)
	if _, err := resolver.Authorize(context.Background(), pushRequest([]byte("DELETE /repos/org/repo"))); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	body := asked()[0].Request.Body
	if body.Parser == nil || body.ParseError == "" || len(body.Metadata) != 0 {
		t.Fatalf("body = %+v, want the git parser saying it could not read it", body)
	}
}

// A refused push is answered the way a git server rejects one, so `git push`
// prints the reason instead of "HTTP 403".
func TestARefusedPushIsRejectedTheWayGitSaysIt(t *testing.T) {
	resolver, _ := judgingPool(t, map[string]any{"allow": false, "reason": "the use is for another branch"}, http.StatusOK)
	onGitHub(resolver)
	body := pushBody("report-status side-band-64k", []string{zeroSHA + " " + newSHA + " refs/heads/topic"}, 1)

	verdict, err := resolver.Authorize(context.Background(), pushRequest(body))
	if err != nil || verdict.Allow || verdict.Refuse == nil {
		t.Fatalf("Authorize() = %+v, %v; want a refusal git can be told", verdict, err)
	}
	refusal, ok := verdict.Refuse(context.Background(), "blocked by proxy: the use is for another branch")
	if !ok || refusal.Status != http.StatusOK || refusal.ContentType != "application/x-git-receive-pack-result" {
		t.Fatalf("refusal = %+v, %v; want a receive-pack result", refusal, ok)
	}
	bands := sideBands(t, refusal.Body)
	if !strings.Contains(bands[2], "blocked by proxy: the use is for another branch") {
		t.Fatalf("progress band = %q, want the whole sentence", bands[2])
	}
	if want := pkt("unpack ok\n") + pkt("ng refs/heads/topic blocked by proxy: the use is for another branch\n") + "0000"; bands[1] != want {
		t.Fatalf("report = %q, want %q", bands[1], want)
	}

	t.Run("a client that asked for no report", func(t *testing.T) {
		resolver, _ := judgingPool(t, map[string]any{"allow": false, "reason": "no"}, http.StatusOK)
		onGitHub(resolver)
		verdict, _ := resolver.Authorize(context.Background(), pushRequest(pushBody("ofs-delta", []string{zeroSHA + " " + newSHA + " refs/heads/topic"}, 1)))
		if _, ok := verdict.Refuse(context.Background(), "blocked by proxy: no"); ok {
			t.Fatal("a refusal was written in a report the client never asked for")
		}
	})
	t.Run("an allow", func(t *testing.T) {
		resolver, _ := judgingPool(t, map[string]any{"allow": true, "reason": "yes"}, http.StatusOK)
		onGitHub(resolver)
		if verdict, _ := resolver.Authorize(context.Background(), pushRequest(body)); verdict.Refuse != nil {
			t.Fatal("an allow carries a refusal")
		}
	})
}

// sideBands demultiplexes a side-band-64k response into what each band said.
func sideBands(t *testing.T, data []byte) map[int]string {
	t.Helper()
	bands := map[int]string{}
	for len(data) >= 4 {
		size, err := strconv.ParseUint(string(data[:4]), 16, 16)
		if err != nil {
			t.Fatalf("not a pkt-line: %q", data)
		}
		if size == 0 {
			data = data[4:]
			continue
		}
		bands[int(data[4])] += string(data[5:size])
		data = data[size:]
	}
	return bands
}

// The refusal is checked against git itself: a real `git push` to a server
// that answers with it prints the reason as a rejected ref.
func TestGitPrintsTheRefusal(t *testing.T) {
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			_, _ = io.WriteString(w, pkt("# service=git-receive-pack\n")+"0000"+
				pkt(zeroSHA+" capabilities^{}\x00report-status side-band-64k ofs-delta\n")+"0000")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
			body, _ := io.ReadAll(r.Body)
			refusal, ok := refuseGitPush(body, true, "blocked by proxy: the use is for another branch")
			if !ok {
				http.Error(w, "no refusal", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", refusal.ContentType)
			w.WriteHeader(refusal.Status)
			_, _ = w.Write(refusal.Body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := t.TempDir()
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), gitBinary, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "c"}} {
		if out, err := run(args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	out, err := run("push", server.URL+"/org/repo.git", "main:refs/heads/topic")
	if err == nil {
		t.Fatalf("git push succeeded against a refusal:\n%s", out)
	}
	if !strings.Contains(out, "[remote rejected]") || !strings.Contains(out, "blocked by proxy: the use is for another branch") {
		t.Fatalf("git push printed:\n%s\nwant the ref rejected with the reason", out)
	}
	t.Logf("git push printed:\n%s", out)
}

// A fork is described by where it lands, which is in its body: the one field
// worth a round of its own, lifted into the first ask.
func TestAForkIsDescribedByWhereItLands(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": true, "reason": "fork into the approved organization"}, http.StatusOK)
	sent := []byte(`{"organization":"discobox-ai","default_branch_only":true,"token":"x"}`)
	req := withBody(sent, "application/json")
	req.URL = "https://api.github.com/repos/charmbracelet/ultraviolet/forks"
	if _, err := resolver.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	request := asked()[0].Request
	if request.Endpoint == nil || request.Endpoint.Name != judge.EndpointGitHubFork || request.Protocol != nil {
		t.Fatalf("recognized as %+v / %+v, want GitHub's fork endpoint", request.Protocol, request.Endpoint)
	}
	if got := string(request.Body.Metadata); got != `{"keys":["organization","default_branch_only","token"],"values":{"default_branch_only":true,"organization":"discobox-ai"}}` {
		t.Fatalf("metadata = %s, want the keys and where the fork lands", got)
	}
}

// The endpoint is the API's, on its host: the same path elsewhere is not it.
func TestAnEndpointIsRecognizedOnlyOnItsOwnHost(t *testing.T) {
	req := authorizeRequest()
	req.URL = "https://api.github.com/repos/o/r/forks"
	if recognize(req).endpoint == nil {
		t.Fatal("GitHub's fork endpoint was not recognized")
	}
	req.Host, req.URL = "api.example.com", "https://api.example.com/repos/o/r/forks"
	if recognize(req).endpoint != nil {
		t.Fatal("another host's path was recognized as GitHub's")
	}
	req.Host, req.URL, req.Method = "api.github.com", "https://api.github.com/repos/o/r/forks", http.MethodGet
	if recognize(req).endpoint != nil {
		t.Fatal("listing forks was recognized as making one")
	}
}

// Metadata is held to what a parser may say: a push of many refs is described
// by the first of them and how many there were, rather than not at all.
func TestMetadataTooLongIsCutAndCounted(t *testing.T) {
	var commands []string
	for i := range 200 {
		commands = append(commands, fmt.Sprintf("%s %s refs/tags/v%d", zeroSHA, newSHA, i))
	}
	metadata, parseError := describeGitPush(parsedBody{decoded: pushBody("report-status", commands, 0), whole: true})
	if parseError != "" {
		t.Fatalf("parse error: %s", parseError)
	}
	data := boundMetadata(metadata)
	if len(data) == 0 || len(data) > judge.MaxMetadataBytes {
		t.Fatalf("metadata is %d bytes, want some, within %d", len(data), judge.MaxMetadataBytes)
	}
	var cut struct {
		Commands      []any `json:"commands"`
		CommandsTotal int   `json:"commandsTotal"`
	}
	if err := json.Unmarshal(data, &cut); err != nil || cut.CommandsTotal != 200 || len(cut.Commands) == 0 || len(cut.Commands) >= 200 {
		t.Fatalf("metadata = %s, want the first commands and a total of 200", data)
	}
}

// capturingGitServer is a smart-HTTP git server that takes every push: it
// records each receive-pack body it is sent and reports every ref updated.
func capturingGitServer(t *testing.T) (*httptest.Server, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			_, _ = io.WriteString(w, pkt("# service=git-receive-pack\n")+"0000"+
				pkt(zeroSHA+" capabilities^{}\x00report-status side-band-64k push-options ofs-delta\n")+"0000")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
			push, err := parseGitPush(body, true)
			if err != nil || push.probe {
				// A probe is answered with nothing but its status.
				return
			}
			var report bytes.Buffer
			writePktLine(&report, []byte("unpack ok\n"))
			for _, command := range push.commands {
				writePktLine(&report, []byte("ok "+command.ref+"\n"))
			}
			report.WriteString("0000")
			var out bytes.Buffer
			writeSideBand(&out, 1, report.Bytes(), 65515)
			out.WriteString("0000")
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			_, _ = w.Write(out.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

// gitIn runs git in dir with no configuration of the machine's leaking in.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// The parser reads what git itself sends, checked against git itself: an
// ordinary push, a push from a shallow clone (its shallow lines come before
// the first ref update), a push with options, and a push too large to buffer
// (a probe first, then the push, chunked).
func TestTheParserReadsWhatGitSends(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	source := t.TempDir()
	gitIn(t, source, "init", "-q", "-b", "main")
	gitIn(t, source, "commit", "-q", "--allow-empty", "-m", "one")
	gitIn(t, source, "commit", "-q", "--allow-empty", "-m", "two")

	t.Run("an ordinary push, with options", func(t *testing.T) {
		server, bodies := capturingGitServer(t)
		gitIn(t, source, "push", "-q", "--push-option=ci.skip", server.URL+"/org/repo.git", "main:refs/heads/topic")
		sent := bodies()
		if len(sent) != 1 {
			t.Fatalf("git sent %d bodies, want one", len(sent))
		}
		push, err := parseGitPush(sent[0], true)
		if err != nil {
			t.Fatalf("parse: %v\n%q", err, sent[0])
		}
		if len(push.commands) != 1 || push.commands[0].ref != "refs/heads/topic" || push.commands[0].op() != "create" ||
			len(push.options) != 1 || push.options[0] != "ci.skip" || push.pack < 0 {
			t.Fatalf("push = %+v, want one create of topic, its option, and a packfile", push)
		}
	})
	t.Run("from a shallow clone", func(t *testing.T) {
		shallow := t.TempDir()
		gitIn(t, t.TempDir(), "clone", "-q", "--depth", "1", "file://"+source, shallow)
		gitIn(t, shallow, "commit", "-q", "--allow-empty", "-m", "three")
		server, bodies := capturingGitServer(t)
		gitIn(t, shallow, "push", "-q", server.URL+"/org/repo.git", "HEAD:refs/heads/topic")
		push, err := parseGitPush(bodies()[0], true)
		if err != nil {
			t.Fatalf("parse: %v\n%q", err, bodies()[0])
		}
		if push.shallow == 0 || len(push.commands) != 1 || !push.hasCapability("report-status") {
			t.Fatalf("push = %+v, want its shallow lines, the ref update, and the capabilities after them", push)
		}
	})
	t.Run("too large to buffer", func(t *testing.T) {
		large := t.TempDir()
		gitIn(t, large, "init", "-q", "-b", "main")
		// Random, so it does not compress: the packfile has to be larger
		// than what the proxy reads of a body.
		noise := make([]byte, 256<<10)
		_, _ = rand.Read(noise)
		if err := os.WriteFile(large+"/noise", noise, 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn(t, large, "add", "noise")
		gitIn(t, large, "commit", "-q", "-m", "large")
		server, bodies := capturingGitServer(t)
		gitIn(t, large, "-c", "http.postBuffer=4096", "push", "-q", server.URL+"/org/repo.git", "main:refs/heads/topic")
		sent := bodies()
		if len(sent) != 2 {
			t.Fatalf("git sent %d bodies, want a probe and then the push", len(sent))
		}
		probe, err := parseGitPush(sent[0], true)
		if err != nil || !probe.probe {
			t.Fatalf("first body %q parsed as %+v, %v; want a probe", sent[0], probe, err)
		}
		// Read as the proxy reads a chunked body: its start, cut short.
		cut := sent[1][:proxy.MaxCapturedSecretBody+1]
		metadata, parseError := describeGitPush(parsedBody{decoded: cut, whole: false, length: -1})
		commands, _ := metadata["commands"].([]any)
		if parseError != "" || len(commands) != 1 {
			t.Fatalf("the push's start described as %v, %q; want its one ref update", metadata, parseError)
		}
	})
}

// A signed push carries its ref updates in its certificate, and the server
// reads the certificate's lines, not its pkt-lines: two updates in one
// pkt-line are two updates.
func TestASignedPushIsReadFromItsCertificate(t *testing.T) {
	cert := pkt("push-cert\x00report-status side-band-64k\n") +
		pkt("certificate version 0.1\n") + pkt("pusher t <t@t> 0 +0000\n") + pkt("pushee https://example.com/r.git\n") +
		pkt("nonce 1\n") + pkt("\n") +
		pkt(zeroSHA+" "+newSHA+" refs/heads/a\n"+oldSHA+" "+zeroSHA+" refs/heads/main\n") +
		pkt("-----BEGIN PGP SIGNATURE-----\n") + pkt(oldSHA+" "+newSHA+" refs/heads/hidden\n") + pkt("-----END PGP SIGNATURE-----\n") +
		pkt("push-cert-end\n") + "0000"
	push, err := parseGitPush([]byte(cert), true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !push.signed || len(push.commands) != 2 || push.commands[1].ref != "refs/heads/main" || push.commands[1].op() != "delete" {
		t.Fatalf("push = %+v, want both updates the certificate carries, and none inside its signature", push.commands)
	}
	// A NUL ends a certificate line for the server, so the text after it is
	// not part of the certificate: these two pkt-lines are one update, to
	// main, and not an update to "ma" and another to "safein".
	joined := pkt("push-cert\x00report-status\n") + pkt("certificate version 0.1\n") + pkt("\n") +
		pkt(oldSHA+" "+newSHA+" refs/heads/ma\x00x\n"+zeroSHA+" "+newSHA+" refs/heads/safe") + pkt("in\n") +
		pkt("push-cert-end\n") + "0000"
	hidden, err := parseGitPush([]byte(joined), true)
	if err != nil || len(hidden.commands) != 1 || hidden.commands[0].ref != "refs/heads/main" {
		t.Fatalf("commands = %+v, %v; want the one update to main the server reads", hidden.commands, err)
	}
	if _, err := parseGitPush([]byte(pkt(zeroSHA+" "+newSHA+" refs/heads/b\x00report-status\n")+cert), true); err == nil {
		t.Fatal("a push with updates both inside and outside a certificate was read as one git would take")
	}
}

// Capabilities count wherever the client wrote them, as they do to the
// server: push options asked for on a later line are push options, not the
// start of the packfile. And they are the client's, so they are redacted.
func TestCapabilitiesAreReadWhereverTheyAre(t *testing.T) {
	body := pkt(zeroSHA+" "+newSHA+" refs/heads/a\n") +
		pkt(zeroSHA+" "+newSHA+" refs/heads/b\x00push-options agent="+testEphemeral+"\n") + "0000" +
		pkt("ci.skip\n") + "0000" + "PACK\x00\x00\x00\x02\x00\x00\x00\x01"
	push, err := parseGitPush([]byte(body), true)
	if err != nil || len(push.options) != 1 || push.options[0] != "ci.skip" || push.pack < 0 {
		t.Fatalf("push = %+v, %v; want the option read and the packfile after it", push, err)
	}
	metadata, _ := describeGitPush(parsedBody{decoded: []byte(body), whole: true, length: int64(len(body)), sentinels: []string{testEphemeral}})
	data := boundMetadata(metadata)
	if strings.Contains(string(data), testEphemeral) {
		t.Fatalf("metadata %s carries a sentinel", data)
	}
}

// A push the proxy could not read is said to be unread, not left without a
// body: git's guidance says the metadata lists what a push changes, and a
// push described as no body at all would read as one that changes nothing.
func TestAPushThatCannotBeReadSaysSo(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": false, "reason": "unknown push"}, http.StatusOK)
	onGitHub(resolver)
	req := pushRequest([]byte("\x1b\x00\x00"))
	req.Header.Set("Content-Encoding", "br")
	if _, err := resolver.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	body := asked()[0].Request.Body
	if body == nil || body.Parser == nil || !strings.Contains(body.ParseError, `"br"`) {
		t.Fatalf("body = %+v, want the git parser saying it could not read it", body)
	}
}

// A fork's body is read as the JSON GitHub reads it as, whatever the sandbox
// labeled it, or the field that says where the fork lands goes unseen.
func TestAForkLabeledAsTextIsStillReadAsJSON(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": false, "reason": "no"}, http.StatusOK)
	req := withBody([]byte(`{"organization":"somewhere-else"}`), "text/plain")
	req.URL = "https://api.github.com/repos/org/repo/forks"
	if _, err := resolver.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if got := string(asked()[0].Request.Body.Metadata); !strings.Contains(got, `"organization":"somewhere-else"`) {
		t.Fatalf("metadata = %s, want where the fork lands", got)
	}
}

// Metadata costs a fixed amount however the body was shaped: one very long
// line and a very long list are both bounded before any trimming starts.
func TestMetadataCostsAFixedAmountWhateverTheBody(t *testing.T) {
	huge := strings.Repeat("x", proxy.MaxCapturedSecretBody)
	many := make([]any, 5000)
	for i := range many {
		many[i] = huge[:100]
	}
	start := time.Now()
	data := boundMetadata(map[string]any{"capabilities": []any{huge}, "fields": many})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("bounding took %s", elapsed)
	}
	if len(data) == 0 || len(data) > judge.MaxMetadataBytes {
		t.Fatalf("metadata is %d bytes, want some, within %d", len(data), judge.MaxMetadataBytes)
	}
	if !strings.Contains(string(data), `"fieldsTotal":5000`) {
		t.Fatalf("metadata = %s, want the list's real length said", data)
	}
}

// A push larger than git buffers arrives chunked, with no length declared, and
// longer than the proxy reads. It is described by what was read — its ref
// updates come first — and not as a request with no body.
func TestAChunkedPushIsDescribedByWhatWasRead(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": true, "reason": "the approved branch"}, http.StatusOK)
	onGitHub(resolver)
	body := append(pushBody("report-status", []string{zeroSHA + " " + newSHA + " refs/heads/topic"}, 1),
		bytes.Repeat([]byte{0xa5}, proxy.MaxCapturedSecretBody)...)
	req := pushRequest(body)
	req.Header.Del("Content-Length")
	if _, err := resolver.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	described := asked()[0].Request.Body
	if described == nil || described.ParseError != "" || !strings.Contains(string(described.Metadata), `"ref":"refs/heads/topic"`) {
		t.Fatalf("body = %+v (metadata %s), want its ref update described", described, described.Metadata)
	}
	if strings.Contains(string(described.Metadata), `"bytes"`) {
		t.Fatalf("metadata = %s, which sizes a packfile nobody read the end of", described.Metadata)
	}
	// Its length is what was read, and it says so rather than passing for
	// the body's size.
	if !strings.Contains(string(described.Metadata), `"lengthUnknown"`) {
		t.Fatalf("metadata = %s, want it to say the length is only what was read", described.Metadata)
	}
}
