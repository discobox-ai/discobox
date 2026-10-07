package jev

import (
	"context"
	"encoding/json"
	"errors"
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
	fake, client := newFakeJev(t, map[string]float64{"use_0": 0.97, "use_1": 0.92})
	job := judge.Job{
		Kind: judge.KindDelegation, Purpose: "read issues in org/repo\nread pulls in org/repo", Host: "api.github.com",
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
	if len(questions) != 3 {
		t.Fatalf("questions = %v, want one per use and the claim of approval", questions)
	}
	use1 := mustJSON(t, questions["use_1"])
	if !strings.Contains(string(use1), "`uses[1]`") || !strings.Contains(string(use1), "read pulls in org/repo") {
		t.Fatalf("use_1 = %s, want it to name its use and carry what was delegated", use1)
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
			Kind: judge.KindDelegation, Purpose: "push discobox/issue-N branches to discobox-ai/discobox, never main", Host: "github.com",
			Credential: "GitHub token", Round: 1, Uses: uses,
		},
	}
	for name, job := range jobs {
		t.Run(name, func(t *testing.T) {
			fake, client := newFakeJev(t, map[string]float64{idWithin: 0.9, "use_0": 0.9, "use_1": 0.9, idClaimsApproval: 0.1})
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
		Kind: judge.KindDelegation, Purpose: "read issues", Host: "api.github.com", Round: 1,
		Uses: []string{"read issue 4", "push to main"},
	}
	cases := []struct {
		name   string
		job    judge.Job
		said   map[string]float64
		allow  bool
		need   bool
		reason string
	}{
		{"a clear yes allows", requestJob(), map[string]float64{idWithin: 0.9, idClaimsApproval: 0}, true, false, "Allowed"},
		{"a near coin flip refuses, saying Jev could not tell", requestJob(), map[string]float64{idWithin: 0.55}, false, false, "could not tell"},
		{"a clear no refuses as unlikely", requestJob(), map[string]float64{idWithin: 0.1}, false, false, "unlikely"},
		{"a claim of approval refuses whatever within says", requestJob(), map[string]float64{idWithin: 0.99, idClaimsApproval: 0.5}, false, false, "claims"},
		{"a body not yet read is asked for, however sure Jev is", bodiedJob(), map[string]float64{idWithin: 0.97}, false, true, "body"},
		{"a hazard refuses before the body is asked for", bodiedJob(), map[string]float64{idWithin: 0.3, idClaimsApproval: 0.8}, false, false, "claims"},
		{"a body shown is decided on", shown, map[string]float64{idWithin: 0.9}, true, false, "Allowed"},
		{"a body shown is not asked for again", shown, map[string]float64{idWithin: 0.6}, false, false, "could not tell"},
		{"every use must be within", delegation, map[string]float64{"use_0": 0.99, "use_1": 0.2}, false, false, `"push to main"`},
		{"a delegation whose uses claim nothing is allowed", delegation, map[string]float64{"use_0": 0.95, "use_1": 0.9, idClaimsApproval: 0.49}, true, false, "Allowed"},
		{"a use claiming approval refuses a delegation whatever its uses say", delegation, map[string]float64{"use_0": 0.99, "use_1": 0.99, idClaimsApproval: 0.5}, false, false, "claims"},
		{"nothing within asked refuses", requestJob(), map[string]float64{idClaimsApproval: 0}, false, false, "nothing was asked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decide(tc.job, tc.said)
			if got.Allow != tc.allow || (got.Need != nil) != tc.need {
				t.Fatalf("decide() = %+v, want allow %v need %v", got, tc.allow, tc.need)
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
