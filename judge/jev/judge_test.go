package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/judge"
)

// fakeJev answers every question with the probability the test set for it,
// and records what it was sent.
type fakeJev struct {
	server *httptest.Server
	mu     sync.Mutex
	asked  []map[string]any
	// bodies are the request bodies as sent, byte for byte.
	bodies [][]byte
	auth   []string
	// answer is each question's probability; an ID it has none for is
	// answered 0.
	answer map[string]float64
	// fail answers with these statuses first, one per request, before
	// answering at all.
	fail []int
	// raw replaces the answer with this body.
	raw string
}

func newFakeJev(t *testing.T, answer map[string]float64) (*fakeJev, *Client) {
	t.Helper()
	fake := &fakeJev{answer: answer}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		fake.mu.Lock()
		fake.asked = append(fake.asked, body)
		fake.bodies = append(fake.bodies, raw)
		fake.auth = append(fake.auth, r.Header.Get("Authorization"))
		var status int
		if len(fake.fail) > 0 {
			status, fake.fail = fake.fail[0], fake.fail[1:]
		}
		fake.mu.Unlock()
		if status != 0 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if fake.raw != "" {
			_, _ = w.Write([]byte(fake.raw))
			return
		}
		answers := map[string]any{}
		for id := range body["questions"].(map[string]any) {
			answers[id] = map[string]any{"type": "noul", "noul": fake.answer[id]}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 1}})
	}))
	t.Cleanup(fake.server.Close)
	client, err := New(Config{APIKey: "ts-key", BaseURL: fake.server.URL, HTTPClient: fake.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return fake, client
}

func (f *fakeJev) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.asked...)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requestJob() judge.Job {
	return judge.Job{
		Kind:       judge.KindRequest,
		Purpose:    "open a pull request in org/repo",
		Host:       "api.github.com",
		Credential: "GitHub token",
		Round:      1,
		Request: &judge.Request{
			Method: http.MethodGet,
			URL:    "https://api.github.com/repos/org/repo",
		},
	}
}

func bodiedJob() judge.Job {
	job := requestJob()
	job.Request.Method = http.MethodPost
	job.Request.URL = "https://api.github.com/repos/org/repo/pulls"
	job.Request.Body = &judge.Body{MediaType: "application/json", Length: 40}
	return job
}

// What a person approved is in the questions and only the evidence is in the
// state: the split that keeps a body arguing for itself one value among the
// evidence rather than words beside the purpose (ADR 26-10-01-324 §3).
func TestTheAuthorizationIsInTheQuestionsAndTheEvidenceInTheState(t *testing.T) {
	fake, client := newFakeJev(t, map[string]float64{idWithin: 0.95})
	job := bodiedJob()
	job.Guidance = []string{"POST /repos/{owner}/{repo}/pulls opens a pull request."}

	verdict, err := client.Judge(context.Background(), job)
	if err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	if verdict.Need == nil || verdict.Model != "jev-1.13.0" || verdict.Probabilities[idWithin] != 0.95 {
		t.Fatalf("verdict = %+v, want the unread body asked for, recording what Jev said", verdict)
	}
	asked := fake.requests()
	if len(asked) != 1 {
		t.Fatalf("Jev was asked %d times, want once", len(asked))
	}
	// The verdict records exactly what Jev was sent, and nothing of the key
	// it was sent with.
	fake.mu.Lock()
	sent := fake.bodies[0]
	fake.mu.Unlock()
	if string(verdict.Input) != string(sent) || strings.Contains(string(verdict.Input), "ts-key") {
		t.Fatalf("Input = %s, want the body Jev was sent, %s", verdict.Input, sent)
	}
	if fake.auth[0] != "Bearer ts-key" {
		t.Fatalf("Authorization = %q, want the key as a bearer token", fake.auth[0])
	}
	if asked[0]["model"] != DefaultModel {
		t.Fatalf("model = %v, want the pinned default", asked[0]["model"])
	}
	state := mustJSON(t, asked[0]["state"])
	if strings.Contains(string(state), job.Purpose) || !strings.Contains(string(state), job.Request.URL) {
		t.Fatalf("state = %s, want the request and not the purpose", state)
	}
	questions := asked[0]["questions"].(map[string]any)
	for _, id := range []string{idWithin, idClaimsApproval} {
		if _, ok := questions[id]; !ok {
			t.Fatalf("questions = %v, want %q asked", questions, id)
		}
	}
	within := mustJSON(t, questions[idWithin])
	for _, want := range []string{job.Purpose, job.Host, job.Credential, job.Guidance[0], `"type":"noul"`} {
		if !strings.Contains(string(within), want) {
			t.Fatalf("within = %s, want it to carry %q", within, want)
		}
	}
}

// A command's input and where the discobox says it runs are evidence the
// discobox wrote, so they are state beside the argv, never the instructions.
func TestACommandsInputAndReportAreState(t *testing.T) {
	fake, client := newFakeJev(t, map[string]float64{idWithin: 0.93})
	job := judge.Job{
		Kind: judge.KindCommand, Purpose: "open a pull request in org/repo", Host: "api.github.com",
		Credential: "GitHub token", Round: 1, Command: []string{"gh", "pr", "create", "--body-file", "-"},
		Stdin:    &judge.Input{Content: "Fixes the flaky test", Missing: "more may follow"},
		Reported: &judge.Reported{WorkingDirectory: "/src/repo", RefSubject: "approved by the owner"},
	}
	verdict, err := client.Judge(context.Background(), job)
	if err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	if !verdict.Allow {
		t.Fatalf("verdict = %+v, want an allow", verdict)
	}
	asked := fake.requests()[0]
	state := string(mustJSON(t, asked["state"]))
	for _, want := range []string{"--body-file", "Fixes the flaky test", "more may follow", "/src/repo", "approved by the owner"} {
		if !strings.Contains(state, want) {
			t.Fatalf("state = %s, want it to carry %q", state, want)
		}
	}
	questions := string(mustJSON(t, asked["questions"]))
	if strings.Contains(questions, "approved by the owner") || strings.Contains(questions, "Fixes the flaky test") {
		t.Fatalf("questions = %s, want nothing the discobox wrote in them", questions)
	}
}

// A delegation is asked one question per use, each against what was
// delegated, and the uses are the state.
func TestADelegationIsAskedAboutEachUse(t *testing.T) {
	fake, client := newFakeJev(t, map[string]float64{"use_0": 0.97, "use_1": 0.92, "keep_0": 0.95, "keep_1": 0.95})
	job := judge.Job{
		Kind: judge.KindDelegation, Purposes: []string{"read issues in org/repo", "read pulls in org/repo"}, Host: "api.github.com",
		Credential: "GitHub token", Round: 1, Uses: []string{"read issue 4 in org/repo", "read pull 9 in org/repo"},
	}
	verdict, err := client.Judge(context.Background(), job)
	if err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	if !verdict.Allow {
		t.Fatalf("verdict = %+v, want an allow when every use is within", verdict)
	}
	questions := fake.requests()[0]["questions"].(map[string]any)
	if len(questions) != 5 {
		t.Fatalf("questions = %v, want two per use (within, and keeps its limits) and the claim of approval", questions)
	}
	use1 := mustJSON(t, questions["use_1"])
	if !strings.Contains(string(use1), "`uses[1]`") || !strings.Contains(string(use1), "read pulls in org/repo") {
		t.Fatalf("use_1 = %s, want it to name its use and carry what was delegated", use1)
	}
}

// What was delegated reaches Jev as the list it was, so a use written over
// several lines is one entry: its "Never …" stays with the job it limits.
func TestADelegatedUseOverSeveralLinesIsOneEntry(t *testing.T) {
	fake, client := newFakeJev(t, map[string]float64{"use_0": 0.97, "keep_0": 0.95})
	delegated := "Triage issues in org/repo.\nNever delete a label."
	job := judge.Job{
		Kind: judge.KindDelegation, Purposes: []string{delegated}, Host: "api.github.com",
		Credential: "GitHub token", Round: 1, Uses: []string{"Triage issue 4 in org/repo. Never delete a label."},
	}
	if _, err := client.Judge(context.Background(), job); err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	var keep struct {
		Instructions struct {
			Delegated []string `json:"delegated"`
		} `json:"instructions"`
	}
	if err := json.Unmarshal(mustJSON(t, fake.requests()[0]["questions"].(map[string]any)["keep_0"]), &keep); err != nil {
		t.Fatal(err)
	}
	if len(keep.Instructions.Delegated) != 1 || keep.Instructions.Delegated[0] != delegated {
		t.Fatalf("delegated = %q, want the one use as it was delegated", keep.Instructions.Delegated)
	}
}

// A rule job is asked one question per use asked for, against the rule's
// uses, which are the authorization and so are in the instructions; the uses
// asked for are an agent's and are the state (ADR 26-10-12-330 §§3, 5).
func TestARuleIsAskedAboutEachUseAskedFor(t *testing.T) {
	fake, client := newFakeJev(t, map[string]float64{"use_0": 0.97, "use_1": 0.3, "keep_0": 0.95, "keep_1": 0.95, idClaimsApproval: 0.02})
	job := judge.Job{ //nolint:gosec // G101: a well-known credential's ID, not a credential.
		Kind: judge.KindRule, Purposes: []string{"read any issue <number> in org/repo", "push a branch discobox/<slug>; never main"}, Host: "github.com",
		Credential: "com.github.api", Round: 1, Uses: []string{"read issue 4 in org/repo", "push to main in org/repo"},
	}
	verdict, err := client.Judge(context.Background(), job)
	if err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	if verdict.Allow || !strings.Contains(verdict.Reason, `"push to main in org/repo"`) || !strings.Contains(verdict.Reason, "rule") {
		t.Fatalf("verdict = %+v, want a refusal naming the use outside the rule", verdict)
	}
	asked := fake.requests()[0]
	questions := asked["questions"].(map[string]any)
	if len(questions) != 5 {
		t.Fatalf("questions = %v, want two per use (within, and keeps its limits) and the claim of approval", questions)
	}
	use1 := string(mustJSON(t, questions["use_1"]))
	if !strings.Contains(use1, "`uses[1]`") || !strings.Contains(use1, "never main") {
		t.Fatalf("use_1 = %s, want it to name its use and carry the rule's uses", use1)
	}
	if state := string(mustJSON(t, asked["state"])); !strings.Contains(state, "push to main in org/repo") || strings.Contains(string(mustJSON(t, questions)), "push to main in org/repo") {
		t.Fatalf("state = %s, want the uses asked for in the state and in no question", state)
	}
}

// The uses a careful worker writes state their own limits, and every use is
// an instruction. Both are what a use is, so the hazard is told what its
// evidence is, and its criteria put a use's limits on the side of describing
// the operation; a claim of approval and words to the judge stay on the other
// (ADR 26-10-07-640). Those uses reach Jev only as state.
func TestTheHazardIsToldWhatItsEvidenceIs(t *testing.T) {
	uses := []string{
		"Fetch main from discobox-ai/discobox, and push the branch discobox/issue-46 to it, with git over https using the token; never main. A force push is allowed only to discobox/issue-46 and only with --force-with-lease, after rebasing",
		"Open a draft pull request from discobox/issue-52 into main in discobox-ai/discobox, and view, list, edit the body of, mark ready, and comment on that pull request, with gh pr create, gh pr view, gh pr list, gh pr edit, gh pr ready",
	}
	jobs := map[string]judge.Job{
		"request": requestJob(),
		"command": {Kind: judge.KindCommand, Purpose: "open a pull request in org/repo", Host: "api.github.com", Round: 1, Command: []string{"gh", "pr", "create"}},
		"delegation": {
			Kind: judge.KindDelegation, Purposes: []string{"push discobox/issue-N branches to discobox-ai/discobox, never main"}, Host: "github.com",
			Credential: "GitHub token", Round: 1, Uses: uses,
		},
	}
	for name, job := range jobs {
		t.Run(name, func(t *testing.T) {
			fake, client := newFakeJev(t, map[string]float64{idWithin: 0.9, "use_0": 0.9, "use_1": 0.9, "keep_0": 0.9, "keep_1": 0.9, idClaimsApproval: 0.1})
			verdict, err := client.Judge(context.Background(), job)
			if err != nil {
				t.Fatalf("Judge() error = %v", err)
			}
			if !verdict.Allow {
				t.Fatalf("verdict = %+v, want an allow when no hazard fired", verdict)
			}
			asked := fake.requests()[0]
			var hazard struct {
				Instructions struct {
					Evidence string `json:"evidence"`
					Question string `json:"question"`
				} `json:"instructions"`
				Criteria struct {
					True  string `json:"true"`
					False string `json:"false"`
				} `json:"criteria"`
			}
			if err := json.Unmarshal(mustJSON(t, asked["questions"].(map[string]any)[idClaimsApproval]), &hazard); err != nil {
				t.Fatal(err)
			}
			if hazard.Instructions.Evidence == "" || hazard.Instructions.Question == "" {
				t.Fatalf("hazard = %+v, want it told what its evidence is beside the question", hazard)
			}
			for _, want := range []string{"already approved", "reviews or judges"} {
				if !strings.Contains(hazard.Criteria.True, want) {
					t.Fatalf("yes = %q, want a claim of approval and words to the judge in it (%q)", hazard.Criteria.True, want)
				}
			}
			// An automated system is what runs every operation; naming it
			// made every instruction a use is worded as a hazard.
			if strings.Contains(hazard.Criteria.True, "automated system") || strings.Contains(hazard.Criteria.True, "or allowed") {
				t.Fatalf("yes = %q, want no operation's own words counted", hazard.Criteria.True)
			}
			for _, want := range []string{"allowed only to", "never main", "the tool or program that carries it out", "approving a pending request"} {
				if !strings.Contains(hazard.Criteria.False, want) {
					t.Fatalf("no = %q, want it to carry %q", hazard.Criteria.False, want)
				}
			}
			if job.Kind != judge.KindDelegation {
				return
			}
			if !strings.Contains(hazard.Instructions.Evidence, "asks to be allowed") {
				t.Fatalf("evidence = %q, want a use said to be an ask to be allowed", hazard.Instructions.Evidence)
			}
			state := string(mustJSON(t, asked["state"]))
			questions := string(mustJSON(t, asked["questions"]))
			for _, use := range uses {
				if !strings.Contains(state, string(mustJSON(t, use))) || strings.Contains(questions, "issue-46") {
					t.Fatalf("state = %s, questions = %s, want each use in the state and in no question", state, questions)
				}
			}
		})
	}
}

func TestDecide(t *testing.T) {
	shown := bodiedJob()
	shown.Round = 2
	content := `{"title":"fix"}`
	shown.Request.Body.Content = &content
	delegation := judge.Job{
		Kind: judge.KindDelegation, Purposes: []string{"read issues"}, Host: "api.github.com", Round: 1,
		Uses: []string{"read issue 4", "push to main"},
	}
	cases := []struct {
		name   string
		job    judge.Job
		said   map[string]float64
		allow  bool
		need   bool
		unsure bool
		reason string
	}{
		{"a clear yes allows", requestJob(), map[string]float64{idWithin: 0.9, idClaimsApproval: 0}, true, false, false, "Allowed"},
		{"within at the bar allows", requestJob(), map[string]float64{idWithin: 0.8, idClaimsApproval: 0.19}, true, false, false, "Allowed"},
		{"a near coin flip refuses, saying Jev could not tell", requestJob(), map[string]float64{idWithin: 0.55}, false, false, true, "could not tell"},
		{"just short of a yes is unsure", requestJob(), map[string]float64{idWithin: 0.79}, false, false, true, "could not tell"},
		{"a no short of sure is unsure", requestJob(), map[string]float64{idWithin: 0.2}, false, false, true, "could not tell"},
		{"a clear no refuses as unlikely", requestJob(), map[string]float64{idWithin: 0.19}, false, false, false, "judged it unlikely"},
		{"a claim of approval Jev is sure of refuses whatever within says", requestJob(), map[string]float64{idWithin: 0.99, idClaimsApproval: 0.8}, false, false, false, "cannot authorize itself"},
		{"a claim of approval Jev half-suspects is unsure whatever within says", requestJob(), map[string]float64{idWithin: 0.99, idClaimsApproval: 0.79}, false, false, true, "could not tell whether the request's own text claims"},
		{"a claim of approval is not set aside short of a sure no", requestJob(), map[string]float64{idWithin: 0.99, idClaimsApproval: 0.2}, false, false, true, "could not tell whether the request's own text claims"},
		{"an unsure hazard is unsure even beside a clear no", requestJob(), map[string]float64{idWithin: 0.05, idClaimsApproval: 0.5}, false, false, true, "could not tell whether the request's own text claims"},
		{"a body not yet read is asked for, however sure Jev is", bodiedJob(), map[string]float64{idWithin: 0.97}, false, true, false, "body"},
		{"a body not yet read is asked for, not refused on its description", bodiedJob(), map[string]float64{idWithin: 0.05}, false, true, false, "body"},
		{"a hazard refuses before the body is asked for", bodiedJob(), map[string]float64{idWithin: 0.3, idClaimsApproval: 0.8}, false, false, false, "cannot authorize itself"},
		{"an unsure hazard is unsure before the body is asked for", bodiedJob(), map[string]float64{idWithin: 0.97, idClaimsApproval: 0.4}, false, false, true, "could not tell"},
		{"a body shown is decided on", shown, map[string]float64{idWithin: 0.9}, true, false, false, "Allowed"},
		{"a body shown is not asked for again", shown, map[string]float64{idWithin: 0.6}, false, false, true, "could not tell"},
		{"a body shown is refused on a clear no", shown, map[string]float64{idWithin: 0.1}, false, false, false, "judged it unlikely"},
		{"every use must be within", delegation, map[string]float64{"use_0": 0.99, "use_1": 0.2}, false, false, true, `could not tell whether "push to main"`},
		{"one use Jev is sure is outside refuses the delegation", delegation, map[string]float64{"use_0": 0.99, "use_1": 0.19}, false, false, false, `judged it unlikely that "push to main"`},
		{"a delegation whose uses claim nothing is allowed", delegation, map[string]float64{"use_0": 0.95, "use_1": 0.9, idClaimsApproval: 0.19}, true, false, false, "Allowed"},
		{"a use that may claim approval leaves a delegation unsure whatever its uses say", delegation, map[string]float64{"use_0": 0.99, "use_1": 0.99, idClaimsApproval: 0.5}, false, false, true, "could not tell"},
		{"a use claiming approval refuses a delegation whatever its uses say", delegation, map[string]float64{"use_0": 0.99, "use_1": 0.99, idClaimsApproval: 0.8}, false, false, false, "cannot authorize itself"},
		{"a use within that drops a limit refuses the delegation, saying so", delegation, map[string]float64{"use_0": 0.99, "use_1": 0.99, "keep_0": 0.95, "keep_1": 0.1}, false, false, false, `judged it unlikely that "push to main" keeps every limit`},
		{"a use Jev is unsure keeps its limits leaves the delegation unsure", delegation, map[string]float64{"use_0": 0.99, "use_1": 0.99, "keep_0": 0.95, "keep_1": 0.5}, false, false, true, `could not tell whether "push to main" keeps every limit`},
		{"uses within that keep their limits allow the delegation", delegation, map[string]float64{"use_0": 0.99, "use_1": 0.99, "keep_0": 0.9, "keep_1": 0.85}, true, false, false, "Allowed"},
		{"nothing within asked refuses", requestJob(), map[string]float64{idClaimsApproval: 0}, false, false, false, "nothing was asked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, unsure := decide(tc.job, tc.said)
			if got.Allow != tc.allow || (got.Need != nil) != tc.need || unsure != tc.unsure {
				t.Fatalf("decide() = %+v unsure %v, want allow %v need %v unsure %v", got, unsure, tc.allow, tc.need, tc.unsure)
			}
			if got.Standing != nil {
				t.Fatalf("decide() let an allow stand: %+v", got.Standing)
			}
			if !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to mention %q", got.Reason, tc.reason)
			}
		})
	}
}

// Every use a delegation hands on or a rule is asked about gets its own
// question about its limits: asked inside within, Jev read a use with its
// "Never …" sentence dropped as within.
func TestEachUseIsAskedWhetherItKeepsItsLimits(t *testing.T) {
	for _, kind := range []string{judge.KindDelegation, judge.KindRule} {
		job := judge.Job{ //nolint:gosec // G101: a well-known credential's ID, not a credential.
			Kind: kind, Purposes: []string{"push a branch discobox/<slug>; never main"}, Host: "github.com",
			Credential: "com.github.api", Round: 1, Uses: []string{"push discobox/x", "push discobox/y"},
		}
		_, questions, err := questionsFor(job)
		if err != nil {
			t.Fatal(err)
		}
		for i := range job.Uses {
			q, ok := questions[fmt.Sprintf("%s%d", idKeep, i)]
			if !ok {
				t.Fatalf("%s: no %s%d question in %v", kind, idKeep, i, questions)
			}
			if text := string(mustJSON(t, q)); !strings.Contains(text, "never main") || !strings.Contains(text, fmt.Sprintf("uses[%d]", i)) {
				t.Fatalf("%s: %s%d = %s, want it to carry the authorization and name its use", kind, idKeep, i, text)
			}
		}
	}
}

// A verdict says whether Jev decided or was unsure, which is what a server
// reads to pass only the unsure ones on (ADR 26-10-12-669 §3).
func TestAVerdictSaysWhetherJevWasUnsure(t *testing.T) {
	for said, unsure := range map[float64]bool{0.9: false, 0.5: true, 0.1: false} {
		_, client := newFakeJev(t, map[string]float64{idWithin: said})
		verdict, err := client.Judge(context.Background(), requestJob())
		if err != nil {
			t.Fatalf("Judge() error = %v", err)
		}
		if verdict.Unsure != unsure || (verdict.Unsure && verdict.Allow) {
			t.Fatalf("verdict at %.1f = %+v, want unsure %v", said, verdict, unsure)
		}
	}
}

// Anything but an answer to every question, as a probability, is no verdict.
func TestAnAnswerThatIsNotEveryProbabilityIsNoVerdict(t *testing.T) {
	cases := map[string]string{
		"missing":      `{"model":"jev-1.13.0","answers":{"within":{"type":"noul","noul":0.9}}}`,
		"wrong type":   `{"model":"jev-1.13.0","answers":{"within":{"type":"choice","choice":"yes"},"claims_approval":{"type":"noul","noul":0}}}`,
		"out of range": `{"model":"jev-1.13.0","answers":{"within":{"type":"noul","noul":1.5},"claims_approval":{"type":"noul","noul":0}}}`,
		"not json":     `allow`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			fake, client := newFakeJev(t, nil)
			fake.raw = raw
			if verdict, err := client.Judge(context.Background(), requestJob()); err == nil {
				t.Fatalf("Judge() = %+v, want no verdict", verdict)
			}
		})
	}
}

// Jev saying it is busy is asked again; Jev refusing the key is not.
func TestABusyJevIsAskedAgainAndARefusedKeyIsNot(t *testing.T) {
	fake, client := newFakeJev(t, map[string]float64{idWithin: 0.9})
	fake.fail = []int{http.StatusTooManyRequests, statusOverloaded}
	if _, err := client.Judge(context.Background(), requestJob()); err != nil {
		t.Fatalf("Judge() error = %v, want an answer once Jev had room", err)
	}
	if n := len(fake.requests()); n != 3 {
		t.Fatalf("Jev was asked %d times, want twice busy and once answered", n)
	}

	fake, client = newFakeJev(t, map[string]float64{idWithin: 0.9})
	fake.fail = []int{http.StatusUnauthorized}
	_, err := client.Judge(context.Background(), requestJob())
	var status *StatusError
	if !errors.As(err, &status) || !status.Unauthorized() || status.Message != "slow down" {
		t.Fatalf("Judge() error = %v, want the refused key with what Jev said", err)
	}
	if n := len(fake.requests()); n != 1 {
		t.Fatalf("Jev was asked %d times, want a refused key asked once", n)
	}
}

func TestAClientNeedsAKey(t *testing.T) {
	if _, err := New(Config{APIKey: "  "}); err == nil {
		t.Fatal("New() made a client with no key")
	}
}

// policyJob is a job under a grant's policy: a branch and its tag may be
// pushed, and nothing force-pushed.
func policyJob(kind string) judge.Job {
	job := requestJob()
	job.Kind = kind
	if kind == judge.KindCommand {
		job.Request, job.Command = nil, []string{"git", "push", "origin", "fix-43", "v1.2.0"}
	}
	job.Purpose = ""
	job.Purposes = []string{
		`allow push on branch "fix-43" in repository "org/repo"`,
		`allow push on tag "v1.2.0" in repository "org/repo"`,
		`deny force-push on anything in anywhere`,
	}
	job.Definitions = map[string]string{"push": "Update a Git ref.", "force-push": "Rewrite a ref.", "branch": "A Git branch.", "tag": "A Git tag.", "repository": "A GitHub repository."}
	return job
}

// A job under a grant's policy asks whether every part of it falls under
// some allow, and whether any part falls under a deny, in one request beside
// the hazard: the allows and the denies each in their own question's
// instructions, and the evidence in the state.
func TestAPolicyIsAskedAboutItsAllowsAndItsDenies(t *testing.T) {
	for _, kind := range []string{judge.KindRequest, judge.KindCommand} {
		t.Run(kind, func(t *testing.T) {
			fake, client := newFakeJev(t, map[string]float64{idAllowed: 0.95, idDenied: 0.05})
			job := policyJob(kind)
			verdict, err := client.Judge(context.Background(), job)
			if err != nil {
				t.Fatalf("Judge() error = %v", err)
			}
			if !verdict.Allow {
				t.Fatalf("verdict = %+v, want an allow", verdict)
			}
			asked := fake.requests()
			if len(asked) != 1 {
				t.Fatalf("Jev was asked %d times, want once", len(asked))
			}
			questions := asked[0]["questions"].(map[string]any)
			if len(questions) != 3 || questions[idWithin] != nil {
				t.Fatalf("questions = %v, want allowed, denied and the hazard", questions)
			}
			allowed := questions[idAllowed].(map[string]any)["instructions"].(map[string]any)
			denied := questions[idDenied].(map[string]any)["instructions"].(map[string]any)
			if got := string(mustJSON(t, allowed["approved_purpose"])); got != string(mustJSON(t, job.Purposes[:2])) || allowed["denies"] != nil {
				t.Fatalf("allowed = %v, want the two allows alone", allowed)
			}
			if got := string(mustJSON(t, denied["denies"])); got != string(mustJSON(t, job.Purposes[2:])) || denied["allows"] != nil {
				t.Fatalf("denied = %v, want the deny alone", denied)
			}
			for _, instructions := range []map[string]any{allowed, denied} {
				if string(mustJSON(t, instructions["definitions"])) != string(mustJSON(t, job.Definitions)) {
					t.Fatalf("instructions = %v, want the definitions", instructions)
				}
			}
			if q := allowed["question"].(string); !strings.Contains(q, "different parts may fall under different entries") {
				t.Fatalf("allowed question = %q, want parts covered by different allows", q)
			}
			if state := string(mustJSON(t, asked[0]["state"])); strings.Contains(state, "Rewrite a ref") || strings.Contains(state, `\"fix-43\" in`) {
				t.Fatalf("state = %s, want none of the policy in it", state)
			}
		})
	}
}

// A policy with no deny asks no deny question.
func TestAPolicyWithNoDenyAsksNoDenyQuestion(t *testing.T) {
	fake, client := newFakeJev(t, map[string]float64{idAllowed: 0.95})
	job := policyJob(judge.KindRequest)
	job.Purposes = job.Purposes[:2]
	delete(job.Definitions, "force-push")
	verdict, err := client.Judge(context.Background(), job)
	if err != nil || !verdict.Allow {
		t.Fatalf("verdict = %+v, %v; want an allow", verdict, err)
	}
	if _, ok := fake.requests()[0]["questions"].(map[string]any)[idDenied]; ok {
		t.Fatal("a policy with no deny was asked about one")
	}
}

// Code combines the two: a deny Jev is sure of refuses whatever the allows
// say, one it cannot rule out is unsure, and an allow needs both.
func TestADenyWinsOverEveryAllow(t *testing.T) {
	job := policyJob(judge.KindCommand)
	for _, tc := range []struct {
		name          string
		allowed, deny float64
		allow, unsure bool
		says          string
	}{
		{"allowed and no deny", 0.95, 0.05, true, false, "within"},
		{"allowed and denied", 0.95, 0.9, false, false, "forbids"},
		{"allowed, a deny not ruled out", 0.95, 0.5, false, true, "forbids"},
		{"not allowed", 0.1, 0.05, false, false, "allow of the policy"},
		{"unsure of the allows", 0.5, 0.05, false, true, "allow of the policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer, unsure := decide(job, map[string]float64{idAllowed: tc.allowed, idDenied: tc.deny, idClaimsApproval: 0.01})
			if answer.Allow != tc.allow || unsure != tc.unsure || !strings.Contains(answer.Reason, tc.says) {
				t.Fatalf("decide = %+v, unsure %v; want allow %v, unsure %v, saying %q", answer, unsure, tc.allow, tc.unsure, tc.says)
			}
		})
	}
}

// A sentence use is asked within, as it was, and not about a policy.
func TestASentenceUseIsNotReadAsAPolicy(t *testing.T) {
	fake, client := newFakeJev(t, map[string]float64{idWithin: 0.95})
	if _, err := client.Judge(context.Background(), requestJob()); err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	questions := fake.requests()[0]["questions"].(map[string]any)
	if len(questions) != 2 || questions[idWithin] == nil || strings.Contains(string(mustJSON(t, questions)), "definitions") {
		t.Fatalf("questions = %v, want within and the hazard, with no word of a policy", questions)
	}
}
