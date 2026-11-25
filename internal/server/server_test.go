package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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

func newHarness(t *testing.T, cfg jobs.Config, token string) *harness {
	t.Helper()
	f := &fakeLLM{}
	mgr := jobs.NewManager(jobs.Deps{Chatter: f, Checker: fakeLean{}, Models: llm.DefaultModels()}, cfg)
	srv := &Server{Token: token, Log: discardLogger(), Heartbeat: 50 * time.Millisecond}
	srv.SetManager(mgr)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
		ts.Close()
	})
	return &harness{t: t, ts: ts, mgr: mgr, llm: f, tok: token}
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
	h := newHarness(t, jobs.Config{}, "")
	v := h.submit(`{"input": "a; b"}`)
	if v.ID == "" || v.Options.Attempts != 6 || v.BudgetUSD != 0.25 {
		t.Fatalf("view = %+v", v)
	}
	done := h.waitState(v.ID, jobs.StateDone)
	if len(done.Statements) != 2 || done.Statements[1].Status != pipeline.StatusProved || !strings.Contains(done.LeanFile, "theorem stmt_2") {
		t.Fatalf("done = %+v", done)
	}
	if done.CostUSD != 0.02 {
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
	h := newHarness(t, jobs.Config{}, "")
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
	h := newHarness(t, jobs.Config{}, "")
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
	h := newHarness(t, jobs.Config{MaxRunning: 1, MaxQueued: 1}, "")
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
	h := newHarness(t, jobs.Config{MaxInputBytes: 10}, "")
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

func TestAuth(t *testing.T) {
	h := newHarness(t, jobs.Config{}, "s3cret")
	h.tok = "wrong"
	if code := h.do("GET", "/v1/jobs", "", nil); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	h.tok = ""
	if code := h.do("GET", "/healthz", "", nil); code != 200 {
		t.Errorf("healthz should not need auth: %d", code)
	}
	h.tok = "s3cret"
	if code := h.do("GET", "/v1/jobs", "", nil); code != 200 {
		t.Errorf("right token: %d", code)
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
