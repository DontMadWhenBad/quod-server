package auth

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func authWith(s *KeyStore, header string) (*Principal, error) {
	r := httptest.NewRequest("GET", "/v1/jobs", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return s.Authenticate(r)
}

func open(t *testing.T, path string) *KeyStore {
	t.Helper()
	s, err := OpenKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestKeyLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quod.db")
	s := open(t, path)
	if n, err := s.ActiveKeys(); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("db mode: %v %v", fi, err)
	}

	secret, k, err := s.Create("alice", 2.5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "qk_") || len(secret) != 3+52 || !strings.HasPrefix(k.ID, "k_") {
		t.Fatalf("secret=%q key=%+v", secret, k)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), secret) {
		t.Fatal("secret stored in database")
	}
	if _, _, err := s.Create("alice", 0); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate active name: %v", err)
	}

	// a second handle (e.g. the admin tool) sees the same data
	admin := open(t, path)
	p, err := authWith(s, "Bearer "+secret)
	if err != nil || p.ID != "key:"+k.ID || p.Name != "alice" || p.DailyBudgetUSD != 2.5 {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	if _, err := authWith(s, "bearer "+secret); err != nil {
		t.Errorf("scheme should be case-insensitive: %v", err)
	}
	for hdr, want := range map[string]error{
		"":                     ErrNoCredentials,
		"Basic abc":            ErrNoCredentials,
		"Bearer qk_wrong":      ErrInvalid,
		"Bearer " + secret[3:]: ErrInvalid,
	} {
		if _, err := authWith(s, hdr); !errors.Is(err, want) {
			t.Errorf("%q: err=%v want %v", hdr, err, want)
		}
	}

	// revoke through the other handle: effective on the very next request
	if _, err := admin.Revoke("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := authWith(s, "Bearer "+secret); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revoked key accepted: %v", err)
	}
	if _, err := admin.Revoke("alice"); err == nil {
		t.Error("revoking twice should fail")
	}
	keys, err := s.List()
	if err != nil || len(keys) != 1 || keys[0].Active() || keys[0].Revoked == nil {
		t.Errorf("keys=%+v err=%v", keys, err)
	}
	// the name can be reused after revocation; revoke by ID works too
	_, k2, err := s.Create("alice", 0)
	if err != nil {
		t.Fatalf("reuse after revoke: %v", err)
	}
	if _, err := s.Revoke(k2.ID); err != nil {
		t.Errorf("revoke by id: %v", err)
	}
}

func TestReopenKeepsKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quod.db")
	s, err := OpenKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.Create("bob", 0)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2 := open(t, path) // migrations are idempotent
	if p, err := authWith(s2, "Bearer "+secret); err != nil || p.Name != "bob" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestNewerSchemaRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quod.db")
	s := open(t, path)
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenKeyStore(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v", err)
	}
}
