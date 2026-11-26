// Package auth identifies API callers. Each authenticated request carries a
// Principal. Server-issued access keys are the first Authenticator; others
// (e.g. OAuth sessions) can be added later without touching job or budget
// code, which only sees Principals.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Principal is an authenticated caller.
type Principal struct {
	// ID is stable and unique across authenticators, e.g. "key:k_3f9a1c2b"
	// (later "github:12345"). Jobs are owned by ID.
	ID   string `json:"id"`
	Name string `json:"name"`
	// DailyBudgetUSD caps this caller's LLM spending per UTC day; 0 means
	// only the server-wide caps apply.
	DailyBudgetUSD float64 `json:"daily_budget_usd,omitempty"`
}

// Anonymous is the principal used when authentication is disabled.
var Anonymous = &Principal{ID: "anonymous", Name: "anonymous"}

var (
	// ErrNoCredentials means the request carries no credentials.
	ErrNoCredentials = errors.New("auth: no credentials")
	// ErrInvalid means the credentials are unknown, malformed or revoked.
	ErrInvalid = errors.New("auth: invalid credentials")
)

// Authenticator identifies the caller of a request.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// BearerToken returns the token from "Authorization: Bearer <token>".
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

type ctxKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the request's principal (nil if none).
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}
