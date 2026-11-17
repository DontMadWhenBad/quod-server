// Package llm talks to the DeepSeek chat completions API and wraps the
// prompts used by the prover: splitting input into statements, formalizing
// them in Lean, back-translating, proving with compiler feedback, explaining.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.deepseek.com"

// Model ids (see GET /models).
const (
	ModelFlash = "deepseek-flash"
	ModelPro   = "deepseek-v4-pro"
)

// Effort is DeepSeek's reasoning_effort. EffortNone disables thinking.
type Effort string

const (
	EffortDefault Effort = "" // server default ("high")
	EffortNone    Effort = "none"
	EffortLow     Effort = "low"
	EffortHigh    Effort = "high"
	EffortMax     Effort = "max"
)

// Message is one chat turn. Reasoning from earlier turns is never sent back:
// without tools the API ignores it anyway.
type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

func System(s string) Message    { return Message{Role: "system", Content: s} }
func User(s string) Message      { return Message{Role: "user", Content: s} }
func Assistant(s string) Message { return Message{Role: "assistant", Content: s} }

// Request is one chat completion call.
type Request struct {
	Model       string
	Messages    []Message
	Effort      Effort
	MaxTokens   int      // 0 = server default (64K in thinking mode)
	JSON        bool     // response_format json_object; prompt must mention "json"
	Temperature *float64 // ignored by the API in thinking mode
}

// Usage is token accounting for one call.
type Usage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	CompletionDetails     *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

// ReasoningTokens is the part of CompletionTokens spent thinking.
func (u Usage) ReasoningTokens() int {
	if u.CompletionDetails == nil {
		return 0
	}
	return u.CompletionDetails.ReasoningTokens
}

// Response is the result of one call.
type Response struct {
	Model        string
	Content      string
	Reasoning    string // chain of thought (thinking mode only)
	FinishReason string // stop | length | content_filter | ...
	Usage        Usage
	CostUSD      float64
	Latency      time.Duration
}

// Chatter is the interface the prompt layer depends on (fake it in tests).
type Chatter interface {
	Chat(ctx context.Context, req Request) (*Response, error)
}

// APIError is a non-2xx response or an unusable completion.
type APIError struct {
	Status    int // HTTP status; 0 for in-body failures
	Message   string
	Retryable bool
}

func (e *APIError) Error() string {
	if e.Status == 0 {
		return "deepseek: " + e.Message
	}
	return fmt.Sprintf("deepseek: HTTP %d: %s", e.Status, e.Message)
}

// ErrTruncated means the model hit max_tokens before finishing.
var ErrTruncated = errors.New("deepseek: response truncated (finish_reason=length)")

// Client is a minimal DeepSeek client using net/http.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTP       *http.Client
	MaxRetries int           // retries for 429/5xx/network errors; default 4
	RetryBase  time.Duration // first backoff; default 2s
	Pricing    Pricing       // for Response.CostUSD; default DefaultPricing
	Now        func() time.Time
}

// NewClient returns a client for apiKey with defaults.
func NewClient(apiKey string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		APIKey:  apiKey,
		// No overall timeout: thinking can take minutes and queued requests
		// stream keep-alive lines. Bound calls with ctx instead.
		HTTP:       &http.Client{},
		MaxRetries: 4,
		RetryBase:  2 * time.Second,
		Pricing:    DefaultPricing,
		Now:        time.Now,
	}
}

// NewClientFromEnv reads DEEPSEEK_API_KEY (and optional DEEPSEEK_BASE_URL).
func NewClientFromEnv() (*Client, error) {
	key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if key == "" {
		return nil, errors.New("DEEPSEEK_API_KEY is not set")
	}
	c := NewClient(key)
	if u := os.Getenv("DEEPSEEK_BASE_URL"); u != "" {
		c.BaseURL = strings.TrimRight(u, "/")
	}
	return c, nil
}

type wireRequest struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	ReasoningEffort Effort          `json:"reasoning_effort,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	ResponseFormat  *responseFormat `json:"response_format,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	Stream          bool            `json:"stream"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content          *string `json:"content"`
			ReasoningContent *string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

// Chat performs one completion with retries on transient failures.
func (c *Client) Chat(ctx context.Context, req Request) (*Response, error) {
	if req.Model == "" {
		return nil, errors.New("deepseek: model not set")
	}
	wr := wireRequest{
		Model:           req.Model,
		Messages:        req.Messages,
		ReasoningEffort: req.Effort,
		MaxTokens:       req.MaxTokens,
		Temperature:     req.Temperature,
	}
	if req.JSON {
		wr.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	body, err := json.Marshal(wr)
	if err != nil {
		return nil, err
	}

	maxRetries := c.MaxRetries
	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, retryAfter, err := c.once(ctx, body)
		if err == nil || errors.Is(err, ErrTruncated) {
			// Truncation is not transient; return the partial response so the
			// caller still sees its cost and can decide what to do.
			return resp, err
		}
		lastErr = err
		var ae *APIError
		retryable := ctx.Err() == nil && (!errors.As(err, &ae) || ae.Retryable)
		if !retryable || attempt >= maxRetries {
			return nil, lastErr
		}
		wait := retryAfter
		if wait == 0 {
			base := c.RetryBase
			if base == 0 {
				base = 2 * time.Second
			}
			wait = base << attempt
			wait += time.Duration(rand.Int64N(int64(wait)/2 + 1)) // jitter
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		}
	}
}

func (c *Client) once(ctx context.Context, body []byte) (*Response, time.Duration, error) {
	now := c.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+c.APIKey)

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	hresp, err := hc.Do(hreq)
	if err != nil {
		return nil, 0, err // network error: retryable (unless ctx is done)
	}
	defer hresp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, 32<<20))
	if err != nil {
		return nil, 0, err
	}

	if hresp.StatusCode/100 != 2 {
		ae := &APIError{Status: hresp.StatusCode, Message: errorMessage(raw)}
		switch hresp.StatusCode {
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			ae.Retryable = true
		}
		return nil, retryAfter(hresp.Header.Get("Retry-After")), ae
	}

	// Body may be preceded by keep-alive blank lines; json skips whitespace.
	var wr wireResponse
	if err := json.Unmarshal(bytes.TrimSpace(raw), &wr); err != nil {
		return nil, 0, &APIError{Message: fmt.Sprintf("decoding response: %v", err), Retryable: true}
	}
	if len(wr.Choices) == 0 {
		return nil, 0, &APIError{Message: "response has no choices", Retryable: true}
	}
	ch := wr.Choices[0]
	r := &Response{
		Model:        wr.Model,
		FinishReason: ch.FinishReason,
		Usage:        wr.Usage,
		Latency:      now().Sub(start),
	}
	if ch.Message.Content != nil {
		r.Content = *ch.Message.Content
	}
	if ch.Message.ReasoningContent != nil {
		r.Reasoning = *ch.Message.ReasoningContent
	}
	r.CostUSD = c.pricing().Cost(wr.Model, wr.Usage, start)

	switch ch.FinishReason {
	case "insufficient_system_resource", "aborted":
		return nil, 0, &APIError{Message: "finish_reason=" + ch.FinishReason, Retryable: true}
	case "length":
		return r, 0, ErrTruncated
	}
	return r, 0, nil
}

func (c *Client) pricing() Pricing {
	if c.Pricing == nil {
		return DefaultPricing
	}
	return c.Pricing
}

func errorMessage(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	return s
}

func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}
