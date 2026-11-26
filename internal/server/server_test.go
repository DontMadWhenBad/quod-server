package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"mathprover/internal/auth"
	"mathprover/internal/jobs"
	"mathprover/internal/lean"
	"mathprover/internal/llm"
	"mathprover/internal/pipeline"
)

// fakeLLM answers by task; prove calls block on gate when it is non-nil.
type fakeLLM struct {
	gate  chan struct{}
	mu    sync.Mutex
	calls int
}

var reName = regexp.MustCompile(`Theorem name: (\S+)`)

func (f *fakeLLM) Chat(ctx context.Context, r llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	sys, user := r.Messages[0].Content, r.Messages[1].Content
	switch {
	case strings.Contains(sys, "split a user's"):
		n := strings.Count(user, ";") + 1
		var parts []string
		for i := range n {
			parts = append(parts, fmt.Sprintf("%q", fmt.Sprintf("s%d", i+1)))
		}
		return &llm.Response{Content: `{"statements": [` + strings.Join(parts, ",") + `]}`}, nil
	case strings.Contains(sys, "translate natural-language"):
		name := reName.FindStringSubmatch(user)[1]
		return &llm.Response{Content: "```lean\ntheorem " + name + " : 1 = 1 := by sorry\n```"}, nil
	case strings.Contains(sys, "say in plain mathematical English"):
		return &llm.Response{Content: "one equals one"}, nil
	case strings.Contains(sys, "write tactic proofs"):
		if f.gate != nil {
			select {
			case <-f.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &llm.Response{Content: "```lean\ngood\n```", CostUSD: 0.01}, nil
	default:
		return &llm.Response{Content: "explained"}, nil
	}
}

type fakeLean struct{}

func (fakeLean) CheckStatement(_ context.Context, base *lean.Snapshot, s lean.Statement) (*lean.StatementCheck, error) {
	code := lean.StatementCode(s)
	return &lean.StatementCheck{OK: true, Code: code, Snapshot: base.Extend(code)}, nil
}

func (f fakeLean) ExploreProof(ctx context.Context, base *lean.Snapshot, s lean.Statement, body string) (*lean.Outcome, error) {
	return f.CheckProof(ctx, base, s, body)
}

func (fakeLean) Query(context.Context, *lean.Snapshot, string, time.Duration) (*lean.Result, error) {
	return &lean.Result{}, nil
}

func (fakeLean) CheckProof(_ context.Context, base *lean.Snapshot, s lean.Statement, body string) (*lean.Outcome, error) {
	a := lean.NewAttempt(s, body)
	res := &lean.Result{Snapshot: base.Extend(a.Code)}
	if a.Body == "good" {
		res.Messages = []lean.Message{{Severity: "info", Pos: lean.Pos{Line: a.AxiomsLine}, Data: "HARNESS_AXIOMS_OK " + s.Name + ": []"}}
	} else {
		res.Messages = []lean.Message{{Severity: "error", Pos: lean.Pos{Line: a.BodyLine}, Data: "nope"}}
	}
	return &lean.Outcome{Attempt: a, Result: res, Verdict: lean.Judge(a, res)}, nil
}

type harness struct {
	t   *testing.T
	ts  *httptest.Server
	mgr *jobs.Manager
	llm *fakeLLM
	tok string
}

func newHarness(t *testing.T, cfg jobs.Config, a auth.Authenticator) *harness {
	t.Helper()
	f := &fakeLLM{}
	mgr := jobs.NewManager(jobs.Deps{Chatter: f, Checkers: pipeline.StaticSource{C: fakeLean{}}, Models: llm.DefaultModels()}, cfg)
	srv := &Server{Auth: a, Log: discardLogger(), Heartbeat: 50 * time.Millisecond}
	srv.SetManager(mgr)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
		ts.Close()
	})
	return &harness{t: t, ts: ts, mgr: mgr, llm: f}
}

func (h *harness) do(method, path, body string, out any) int {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.ts.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.tok != "" {
		req.Header.Set("Authorization", "Bearer "+h.tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			h.t.Fatalf("%s %s: decoding %q: %v", method, path, b, err)
		}
	}
	return resp.StatusCode
}

func (h *harness) submit(body string) jobs.View {
	h.t.Helper()
	var v jobs.View
	if code := h.do("POST", "/v1/jobs", body, &v); code != http.StatusAccepted {
		h.t.Fatalf("submit: status %d", code)
	}
	return v
}

func (h *harness) waitState(id string, want jobs.State) jobs.View {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var v jobs.View
		h.do("GET", "/v1/jobs/"+id, "", &v)
		if v.State == want {
			return v
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("job %s: state %s, want %s (error %q)", id, v.State, want, v.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type sseEvent struct {
	id, event string
	data      jobs.Event
}

// readSSE reads events until the "end" event or EOF.
func (h *harness) readSSE(path, lastID string) []sseEvent {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.ts.URL+path, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	if h.tok != "" {
		req.Header.Set("Authorization", "Bearer "+h.tok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		h.t.Fatalf("content-type %q", ct)
	}
	var out []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.event == "end" {
				return out
			}
			if cur.event != "" {
				out = append(out, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			cur.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			cur.event = line[7:]
		case strings.HasPrefix(line, "data: "):
			_ = json.Unmarshal([]byte(line[6:]), &cur.data)
		}
	}
	h.t.Fatalf("stream ended without end event (err %v)", sc.Err())
	return nil
}

func TestJobLifecycleAndSSE(t *testing.T) {
	h := newHarness(t, jobs.Config{}, nil)
	v := h.submit(`{"input": "a; b"}`)
	if v.ID == "" || v.Options.Attempts != 0 || v.BudgetUSD != 0.25 {
		t.Fatalf("view = %+v", v)
	}
	done := h.waitState(v.ID, jobs.StateDone)
	if len(done.Statements) != 2 || done.Statements[1].Status != pipeline.StatusProved || !strings.Contains(done.LeanFile, "theorem stmt_2") {
		t.Fatalf("done = %+v", done)
	}
	// Each statement costs at least one proof call ($0.01); racing sessions
	// in the first stage may add more.
	if done.CostUSD < 0.02 {
		t.Errorf("cost = %g", done.CostUSD)
	}

	evs := h.readSSE("/v1/jobs/"+v.ID+"/events", "")
	types := []string{}
	for _, e := range evs {
		types = append(types, e.event)
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{"state", "split", "formalized", "attempt", "proved", "done"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %s", want, joined)
		}
	}
	if last := evs[len(evs)-1]; last.event != "state" || last.data.State != jobs.StateDone {
		t.Errorf("last event = %+v", last)
	}

	// resume: only events after id 3
	rest := h.readSSE("/v1/jobs/"+v.ID+"/events", "3")
	if len(rest) != len(evs)-3 || rest[0].id != "4" {
		t.Errorf("resume got %d events starting at %s, want %d starting at 4", len(rest), rest[0].id, len(evs)-3)
	}

	var list struct {
		Jobs []struct {
			ID     string `json:"id"`
			Proved int    `json:"proved"`
		} `json:"jobs"`
	}
	h.do("GET", "/v1/jobs", "", &list)
	if len(list.Jobs) != 1 || list.Jobs[0].Proved != 2 {
		t.Errorf("list = %+v", list)
	}
}

func TestLiveStreamWhileRunning(t *testing.T) {
	h := newHarness(t, jobs.Config{}, nil)
	h.llm.gate = make(chan struct{})
	v := h.submit(`{"input": "x", "options": {"no_explain": true}}`)
	got := make(chan []sseEvent, 1)
	go func() { got <- h.readSSE("/v1/jobs/"+v.ID+"/events", "") }()
	time.Sleep(100 * time.Millisecond) // stream is open and waiting at the prove step
	close(h.llm.gate)
	evs := <-got
	if evs[len(evs)-1].data.State != jobs.StateDone {
		t.Errorf("stream did not follow job to completion: %+v", evs[len(evs)-1])
	}
}

func TestConfirmFlow(t *testing.T) {
	h := newHarness(t, jobs.Config{}, nil)
	v := h.submit(`{"input": "x", "options": {"confirm": true, "no_split": true}}`)
	aw := h.waitState(v.ID, jobs.StateAwaiting)
	if aw.Pending == nil || aw.Pending.Signature != ": 1 = 1" || aw.Pending.BackTranslation != "one equals one" {
		t.Fatalf("pending = %+v", aw.Pending)
	}
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	if code := h.do("POST", "/v1/jobs/"+v.ID+"/confirm", `{"stmt": 2, "accept": true}`, &e); code != 409 || e.Error.Code != "no_pending_confirmation" {
		t.Errorf("wrong stmt: %d %+v", code, e)
	}
	if code := h.do("POST", "/v1/jobs/"+v.ID+"/confirm", `{"stmt": 1, "accept": true}`, nil); code != 200 {
		t.Fatalf("confirm: %d", code)
	}
	h.waitState(v.ID, jobs.StateDone)
	if code := h.do("POST", "/v1/jobs/"+v.ID+"/confirm", `{"stmt": 1, "accept": true}`, nil); code != 409 {
		t.Errorf("second confirm: %d", code)
	}
}

func TestCancelQueuedAndRunning(t *testing.T) {
	h := newHarness(t, jobs.Config{MaxRunning: 1, MaxQueued: 1}, nil)
	h.llm.gate = make(chan struct{}) // proofs block forever
	running := h.submit(`{"input": "x"}`)
	h.waitState(running.ID, jobs.StateRunning)
	queued := h.submit(`{"input": "y"}`)
	if queued.State != jobs.StateQueued {
		t.Fatalf("second job state %s", queued.State)
	}
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	if code := h.do("POST", "/v1/jobs", `{"input": "z"}`, &e); code != 429 || e.Error.Code != "queue_full" {
		t.Errorf("third job: %d %+v", code, e)
	}
	h.do("POST", "/v1/jobs/"+queued.ID+"/cancel", "", nil)
	h.do("POST", "/v1/jobs/"+running.ID+"/cancel", "", nil)
	if v := h.waitState(queued.ID, jobs.StateCanceled); v.Error != "canceled by client" {
		t.Errorf("queued: %+v", v)
	}
	h.waitState(running.ID, jobs.StateCanceled)
}

func TestValidationAndErrors(t *testing.T) {
	h := newHarness(t, jobs.Config{MaxInputBytes: 10}, nil)
	cases := []struct {
		body string
		code int
	}{
		{`{"input": ""}`, 400},
		{`{"input": "this is way too long"}`, 400},
		{`{"input": "x", "bogus": 1}`, 400},
		{`not json`, 400},
		{`{"input": "x", "options": {"attempts": -1}}`, 400},
	}
	for _, c := range cases {
		if code := h.do("POST", "/v1/jobs", c.body, nil); code != c.code {
			t.Errorf("%s: status %d, want %d", c.body, code, c.code)
		}
	}
	if code := h.do("GET", "/v1/jobs/nope", "", nil); code != 404 {
		t.Errorf("missing job: %d", code)
	}
	// options are clamped to the manager's limits
	v := h.submit(`{"input": "x", "options": {"attempts": 99, "parallel": 99, "budget_usd": 50}}`)
	if v.Options.Attempts != 12 || v.Options.Parallel != 4 || v.BudgetUSD != 1 {
		t.Errorf("not clamped: %+v budget=%g", v.Options, v.BudgetUSD)
	}
}

func TestAccessKeys(t *testing.T) {
	store, err := auth.OpenKeyStore(filepath.Join(t.TempDir(), "quod.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alice, _, err := store.Create("alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := store.Create("bob", 0)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, jobs.Config{}, store)

	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if code := h.do("GET", "/v1/jobs", "", &e); code != 401 || !strings.Contains(e.Error.Message, "missing access key") {
		t.Errorf("no key: %d %+v", code, e)
	}
	h.tok = "qk_notarealkey"
	if code := h.do("GET", "/v1/jobs", "", &e); code != 401 || !strings.Contains(e.Error.Message, "invalid") {
		t.Errorf("bad key: %d %+v", code, e)
	}
	h.tok = ""
	if code := h.do("GET", "/healthz", "", nil); code != 200 {
		t.Errorf("healthz should not need a key: %d", code)
	}

	// alice's job is hers alone
	h.tok = alice
	v := h.submit(`{"input": "x", "options": {"no_explain": true}}`)
	if v.Owner != "alice" {
		t.Errorf("owner = %q", v.Owner)
	}
	h.waitState(v.ID, jobs.StateDone)
	h.tok = bob
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/jobs/" + v.ID},
		{"GET", "/v1/jobs/" + v.ID + "/events"},
		{"POST", "/v1/jobs/" + v.ID + "/cancel"},
		{"POST", "/v1/jobs/" + v.ID + "/confirm"},
	} {
		body := ""
		if strings.HasSuffix(c.path, "/confirm") {
			body = `{"stmt": 1, "accept": true}`
		}
		if code := h.do(c.method, c.path, body, nil); code != 404 {
			t.Errorf("bob %s %s: %d, want 404", c.method, c.path, code)
		}
	}
	var list struct {
		Jobs []struct{ ID string } `json:"jobs"`
	}
	h.do("GET", "/v1/jobs", "", &list)
	if len(list.Jobs) != 0 {
		t.Errorf("bob sees alice's jobs: %+v", list)
	}
	h.tok = alice
	h.do("GET", "/v1/jobs", "", &list)
	if len(list.Jobs) != 1 {
		t.Errorf("alice's list: %+v", list)
	}

	// revoking takes effect on the next request, without a restart
	if _, err := store.Revoke("alice"); err != nil {
		t.Fatal(err)
	}
	if code := h.do("GET", "/v1/jobs", "", nil); code != 401 {
		t.Fatalf("revoked key: %d", code)
	}
}

func TestPerKeyDailyBudget(t *testing.T) {
	store, err := auth.OpenKeyStore(filepath.Join(t.TempDir(), "quod.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	k, _, err := store.Create("small", 0.015) // one job costs >= $0.01
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, jobs.Config{}, store)
	h.tok = k
	v := h.submit(`{"input": "x", "options": {"no_explain": true, "parallel": 1}}`)
	if v.BudgetUSD > 0.015 {
		t.Errorf("job budget %g exceeds the key's daily budget", v.BudgetUSD)
	}
	h.waitState(v.ID, jobs.StateDone)
	h.do("POST", "/v1/jobs", `{"input": "y"}`, nil) // may still fit
	deadline := time.Now().Add(5 * time.Second)
	for {
		var e struct {
			Error struct{ Code string } `json:"error"`
		}
		code := h.do("POST", "/v1/jobs", `{"input": "z", "options": {"no_explain": true}}`, &e)
		if code == 429 && e.Error.Code == "daily_budget_exhausted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("per-key budget never enforced (last status %d)", code)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestNotReady(t *testing.T) {
	srv := &Server{Log: discardLogger()}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for path, want := range map[string]int{"/v1/jobs": 503, "/readyz": 503, "/healthz": 200} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestReadyzRevealsNothing(t *testing.T) {
	h := newHarness(t, jobs.Config{}, nil)
	var body map[string]any
	if code := h.do("GET", "/readyz", "", &body); code != 200 || len(body) != 1 || body["ready"] != true {
		t.Errorf("readyz = %d %v", code, body)
	}
}

func TestStatusShowsOwnUsage(t *testing.T) {
	store, err := auth.OpenKeyStore(filepath.Join(t.TempDir(), "quod.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	k, _, _ := store.Create("alice", 3)
	h := newHarness(t, jobs.Config{}, store)
	h.tok = k
	v := h.submit(`{"input": "x", "options": {"no_explain": true}}`)
	h.waitState(v.ID, jobs.StateDone)
	var st struct {
		Principal struct{ Name string }
		Usage     jobs.Usage
		Server    struct {
			MaxRunning int `json:"max_running"`
		}
	}
	if code := h.do("GET", "/v1/status", "", &st); code != 200 {
		t.Fatalf("status %d", code)
	}
	if st.Principal.Name != "alice" || st.Usage.SpentTodayUSD <= 0 || st.Usage.DailyBudgetUSD != 3 ||
		st.Usage.RemainingUSD == nil || *st.Usage.RemainingUSD >= 3 || st.Usage.MaxActiveJobs != 2 || st.Server.MaxRunning != 2 {
		t.Errorf("status = %+v", st)
	}
	h.tok = ""
	if code := h.do("GET", "/v1/status", "", nil); code != 401 {
		t.Errorf("status without key: %d", code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t, jobs.Config{}, nil)
	resp, err := http.Get(h.ts.URL + "/v1/jobs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(resp.Header.Get("X-Request-ID")) != 16 || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", resp.Header)
	}
}

// limitedHarness is a harness with tight limits.
func limitedHarness(t *testing.T, a auth.Authenticator, l Limits, cfg jobs.Config) *harness {
	t.Helper()
	f := &fakeLLM{}
	mgr := jobs.NewManager(jobs.Deps{Chatter: f, Checkers: pipeline.StaticSource{C: fakeLean{}}, Models: llm.DefaultModels()}, cfg)
	srv := &Server{Auth: a, Log: discardLogger(), Heartbeat: 50 * time.Millisecond, Limits: l}
	srv.SetManager(mgr)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
		ts.Close()
	})
	return &harness{t: t, ts: ts, mgr: mgr, llm: f}
}

func TestUnauthenticatedFloodIsThrottled(t *testing.T) {
	store, _ := auth.OpenKeyStore(filepath.Join(t.TempDir(), "quod.db"))
	defer store.Close()
	good, _, _ := store.Create("alice", 0)
	h := limitedHarness(t, store, Limits{UnauthPerMin: 1, UnauthBurst: 3}, jobs.Config{})
	h.tok = "qk_wrong"
	for i := range 3 {
		if code := h.do("GET", "/v1/jobs", "", nil); code != 401 {
			t.Fatalf("bad key %d: %d", i, code)
		}
	}
	req, _ := http.NewRequest("GET", h.ts.URL+"/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+good)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// even a valid key from the same IP is refused while the IP is throttled
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("throttled IP: %d retry-after=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if code := h.do("GET", "/healthz", "", nil); code != 200 {
		t.Errorf("healthz must stay reachable: %d", code)
	}
}

func TestPerKeyRateAndCaps(t *testing.T) {
	store, _ := auth.OpenKeyStore(filepath.Join(t.TempDir(), "quod.db"))
	defer store.Close()
	k, _, _ := store.Create("alice", 0)
	other, _, _ := store.Create("bob", 0)

	// request rate per key
	h := limitedHarness(t, store, Limits{KeyPerMin: 1, KeyBurst: 2}, jobs.Config{})
	h.tok = k
	h.do("GET", "/v1/jobs", "", nil)
	h.do("GET", "/v1/jobs", "", nil)
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	if code := h.do("GET", "/v1/jobs", "", &e); code != 429 || e.Error.Code != "rate_limited" {
		t.Errorf("third request: %d %+v", code, e)
	}
	h.tok = other
	if code := h.do("GET", "/v1/jobs", "", nil); code != 200 {
		t.Errorf("limits are per key: %d", code)
	}

	// active jobs and open streams per key
	h2 := limitedHarness(t, store, Limits{MaxStreamsPerKey: 1}, jobs.Config{MaxActivePerKey: 1})
	h2.llm.gate = make(chan struct{}) // keep the job running
	h2.tok = k
	v := h2.submit(`{"input": "x"}`)
	if code := h2.do("POST", "/v1/jobs", `{"input": "y"}`, &e); code != 429 || e.Error.Code != "too_many_jobs" {
		t.Errorf("second active job: %d %+v", code, e)
	}
	h2.tok = other
	h2.submit(`{"input": "z"}`) // another key is unaffected
	h2.tok = k

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h2.ts.URL+"/v1/jobs/"+v.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+k)
	resp, err := http.DefaultClient.Do(req) // first stream stays open
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if code := h2.do("GET", "/v1/jobs/"+v.ID+"/events", "", &e); code != 429 || e.Error.Code != "too_many_streams" {
		t.Errorf("second stream: %d %+v", code, e)
	}
	close(h2.llm.gate)
}

func TestCORSHeaders(t *testing.T) {
	store, _ := auth.OpenKeyStore(filepath.Join(t.TempDir(), "quod.db"))
	defer store.Close()
	k, _, _ := store.Create("web", 0)
	cors, err := ParseCORSOrigins("https://me.github.io")
	if err != nil {
		t.Fatal(err)
	}
	mgr := jobs.NewManager(jobs.Deps{Chatter: &fakeLLM{}, Checkers: pipeline.StaticSource{C: fakeLean{}}, Models: llm.DefaultModels()}, jobs.Config{})
	srv := &Server{Auth: store, Log: discardLogger(), CORS: cors, Limits: Limits{UnauthPerMin: 1, UnauthBurst: 1}}
	srv.SetManager(mgr)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer mgr.Shutdown(context.Background())

	do := func(method, origin, authz string, hdr map[string]string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+"/v1/status", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if authz != "" {
			req.Header.Set("Authorization", "Bearer "+authz)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	// preflights never hit auth or rate limits, however many arrive
	for range 3 {
		resp := do("OPTIONS", "https://me.github.io", "", map[string]string{
			"Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "authorization"})
		if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "https://me.github.io" ||
			!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
			t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
		}
	}
	// actual request from the allowed origin gets CORS headers, credentials never
	resp := do("GET", "https://me.github.io", k, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "https://me.github.io" ||
		resp.Header.Get("Access-Control-Allow-Credentials") != "" || !strings.Contains(resp.Header.Get("Access-Control-Expose-Headers"), "X-Request-ID") {
		t.Errorf("allowed origin: %d %v", resp.StatusCode, resp.Header)
	}
	// other origins get no CORS headers (the browser blocks them)
	resp = do("GET", "https://evil.example", k, nil)
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("foreign origin got CORS headers: %v", resp.Header)
	}
	resp = do("OPTIONS", "https://evil.example", "", map[string]string{"Access-Control-Request-Method": "GET"})
	if resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.StatusCode == 204 {
		t.Errorf("foreign preflight: %d %v", resp.StatusCode, resp.Header)
	}
}
