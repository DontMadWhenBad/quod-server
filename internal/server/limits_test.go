package server

import (
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	trust, err := ParseTrustedProxies("127.0.0.1, ::1/128, 10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	none, _ := ParseTrustedProxies("")
	cases := []struct {
		name   string
		trust  ProxyTrust
		remote string
		cf     string
		xff    []string
		want   string
	}{
		{"direct, no proxies trusted", none, "203.0.113.5:4444", "", nil, "203.0.113.5"},
		{"spoofed XFF from untrusted peer is ignored", trust, "203.0.113.5:4444", "1.2.3.4", []string{"1.2.3.4"}, "203.0.113.5"},
		{"headers ignored when nothing is trusted", none, "127.0.0.1:1", "1.2.3.4", nil, "127.0.0.1"},
		{"CF-Connecting-IP from trusted proxy", trust, "127.0.0.1:5555", "198.51.100.7", nil, "198.51.100.7"},
		{"IPv6 loopback proxy", trust, "[::1]:5555", "2001:db8::1", nil, "2001:db8::1"},
		{"right-most untrusted XFF hop", trust, "127.0.0.1:5555", "", []string{"6.6.6.6, 198.51.100.9, 10.1.2.3"}, "198.51.100.9"},
		{"XFF across several headers", trust, "127.0.0.1:5555", "", []string{"6.6.6.6", "198.51.100.9"}, "198.51.100.9"},
		{"malformed XFF falls back to peer", trust, "127.0.0.1:5555", "", []string{"garbage"}, "127.0.0.1"},
		{"bad CF header, use XFF", trust, "127.0.0.1:5555", "nope", []string{"198.51.100.1"}, "198.51.100.1"},
		{"IPv4-mapped peer", trust, "[::ffff:127.0.0.1]:5555", "198.51.100.8", nil, "198.51.100.8"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.cf != "" {
			r.Header.Set("CF-Connecting-IP", c.cf)
		}
		for _, x := range c.xff {
			r.Header.Add("X-Forwarded-For", x)
		}
		if got := c.trust.ClientIP(r); got != netip.MustParseAddr(c.want) {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if _, err := ParseTrustedProxies("not-an-ip"); err == nil {
		t.Error("bad proxy accepted")
	}
}

func TestLimiterTable(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lt := newLimiterTable(60, 2, 3) // 1 token/s, burst 2, max 3 keys
	lt.now = func() time.Time { return now }

	for i := range 2 {
		if ok, _ := lt.allow("a"); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	ok, wait := lt.allow("a")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("third request: ok=%v wait=%v", ok, wait)
	}
	if over, _ := lt.exhausted("a"); !over {
		t.Error("exhausted should report true")
	}
	if over, _ := lt.exhausted("b"); over {
		t.Error("fresh key should not be exhausted")
	}
	now = now.Add(time.Second)
	if ok, _ := lt.allow("a"); !ok {
		t.Error("token should refill after 1s")
	}

	// table full: further keys share the overflow bucket
	lt.allow("c")
	if lt.size() != 3 {
		t.Fatalf("size = %d", lt.size())
	}
	lt.allow("d")
	lt.allow("e")
	if lt.size() != 3 {
		t.Errorf("table grew past max: %d", lt.size())
	}
	if ok, _ := lt.allow("f"); ok {
		t.Error("overflow bucket should be exhausted after d and e")
	}

	// idle keys are evicted
	now = now.Add(idleAfter + 2*time.Minute)
	lt.allow("g")
	if lt.size() != 1 {
		t.Errorf("idle keys not evicted: size %d", lt.size())
	}
}

func TestCounter(t *testing.T) {
	c := newCounter(2)
	if !c.acquire("k") || !c.acquire("k") || c.acquire("k") {
		t.Fatal("cap not enforced")
	}
	if !c.acquire("other") {
		t.Error("caps are per key")
	}
	c.release("k")
	if !c.acquire("k") {
		t.Error("release should free a slot")
	}
}
