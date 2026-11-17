package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient("test-key")
	c.BaseURL = srv.URL
	c.RetryBase = time.Millisecond
	// Fixed off-peak time (Saturday) for deterministic costs.
	c.Now = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }
	return c
}

const okBody = `{"model":"deepseek-flash","choices":[{"message":{"role":"assistant","content":"hello","reasoning_content":"thinking..."},"finish_reason":"stop"}],
"usage":{"prompt_tokens":1000,"completion_tokens":2000,"total_tokens":3000,"prompt_cache_hit_tokens":400,"prompt_cache_miss_tokens":600,"completion_tokens_details":{"reasoning_tokens":1500}}}`

func TestChatRequestAndResponse(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("path=%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != ModelFlash || body["reasoning_effort"] != "low" || body["stream"] != false {
			t.Errorf("body = %v", body)
		}
		if rf, _ := body["response_format"].(map[string]any); rf["type"] != "json_object" {
			t.Errorf("response_format = %v", body["response_format"])
		}
		if _, ok := body["max_tokens"]; ok {
			t.Error("max_tokens should be omitted when 0")
		}
		// keep-alive blank lines before the body, as DeepSeek sends while queued
		io.WriteString(w, "\n\n\n"+okBody)
	})
	resp, err := c.Chat(context.Background(), Request{Model: ModelFlash, Effort: EffortLow, JSON: true, Messages: []Message{User("hi json")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" || resp.Reasoning != "thinking..." || resp.Usage.ReasoningTokens() != 1500 {
		t.Errorf("resp = %+v", resp)
	}
	// 400*0.003 + 600*0.15 + 2000*0.6 per 1M
	want := (400*0.003 + 600*0.15 + 2000*0.6) / 1e6
	if diff := resp.CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost = %g, want %g", resp.CostUSD, want)
	}
}

func TestChatRetriesTransient(t *testing.T) {
	var n atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"slow down"}}`)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			io.WriteString(w, okBody)
		}
	})
	resp, err := c.Chat(context.Background(), Request{Model: ModelFlash, Messages: []Message{User("x")}})
	if err != nil || resp.Content != "hello" || n.Load() != 3 {
		t.Fatalf("err=%v calls=%d", err, n.Load())
	}
}

func TestChatDoesNotRetryClientErrors(t *testing.T) {
	for _, status := range []int{400, 401, 402, 422} {
		var n atomic.Int32
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"nope","type":"invalid_request_error"}}`)
		})
		_, err := c.Chat(context.Background(), Request{Model: ModelFlash, Messages: []Message{User("x")}})
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != status || ae.Message != "nope" || n.Load() != 1 {
			t.Errorf("status %d: err=%v calls=%d", status, err, n.Load())
		}
	}
}

func TestChatGivesUpAfterMaxRetries(t *testing.T) {
	var n atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	c.MaxRetries = 2
	_, err := c.Chat(context.Background(), Request{Model: ModelFlash, Messages: []Message{User("x")}})
	if err == nil || n.Load() != 3 {
		t.Fatalf("err=%v calls=%d", err, n.Load())
	}
}

func TestChatTruncatedNotRetried(t *testing.T) {
	var n atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		io.WriteString(w, strings.Replace(okBody, `"stop"`, `"length"`, 1))
	})
	resp, err := c.Chat(context.Background(), Request{Model: ModelFlash, Messages: []Message{User("x")}})
	if !errors.Is(err, ErrTruncated) || resp == nil || resp.CostUSD == 0 || n.Load() != 1 {
		t.Fatalf("err=%v resp=%v calls=%d", err, resp, n.Load())
	}
}

func TestChatContextCancel(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	c.RetryBase = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Chat(ctx, Request{Model: ModelFlash, Messages: []Message{User("x")}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestPricing(t *testing.T) {
	u := Usage{PromptCacheMissTokens: 1_000_000, CompletionTokens: 1_000_000}
	sat := time.Date(2026, 11, 14, 2, 0, 0, 0, time.UTC) // weekend
	mon := time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC) // Monday 02:00 UTC = peak
	monOff := time.Date(2026, 11, 16, 5, 0, 0, 0, time.UTC)
	if got := DefaultPricing.Cost(ModelFlash, u, sat); got != 0.75 {
		t.Errorf("weekend = %g", got)
	}
	if got := DefaultPricing.Cost(ModelFlash, u, mon); got != 1.5 {
		t.Errorf("peak = %g", got)
	}
	if got := DefaultPricing.Cost(ModelFlash, u, monOff); got != 0.75 {
		t.Errorf("off-peak weekday = %g", got)
	}
	if got := DefaultPricing.Cost("deepseek-v4-pro-0813", u, sat); got != 0.66+1.98 {
		t.Errorf("versioned id = %g", got)
	}
	if got := DefaultPricing.Cost("other", u, sat); got != 0 {
		t.Errorf("unknown = %g", got)
	}
}
