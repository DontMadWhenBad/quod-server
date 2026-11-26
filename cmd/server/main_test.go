package main

import "testing"

func TestCheckAuthMode(t *testing.T) {
	for _, c := range []struct {
		mode, addr string
		ok         bool
	}{
		{"keys", "0.0.0.0:8080", true},
		{"none", "127.0.0.1:8080", true},
		{"none", "[::1]:8080", true},
		{"none", "localhost:8080", true},
		{"none", "0.0.0.0:8080", false},
		{"none", ":8080", false},
		{"none", "100.87.241.116:8080", false},
		{"bogus", "127.0.0.1:8080", false},
	} {
		if err := checkAuthMode(c.mode, c.addr); (err == nil) != c.ok {
			t.Errorf("checkAuthMode(%q, %q) = %v", c.mode, c.addr, err)
		}
	}
}
