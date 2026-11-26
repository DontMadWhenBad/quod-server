package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CORS lets browser clients on other origins (e.g. a GitHub Pages site)
// call the API. Only exact, configured origins are allowed. No cookies are
// used (the access key travels in the Authorization header), so
// credentials are never allowed.
type CORS struct {
	origins map[string]bool
}

// ParseCORSOrigins parses a comma-separated list of origins such as
// "https://user.github.io,http://localhost:8000". Empty = no CORS.
func ParseCORSOrigins(s string) (CORS, error) {
	c := CORS{origins: map[string]bool{}}
	for _, o := range strings.Split(s, ",") {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o == "" {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" {
			return CORS{}, fmt.Errorf("CORS origin %q must look like https://host[:port]", o)
		}
		c.origins[strings.ToLower(u.Scheme+"://"+u.Host)] = true
	}
	return c, nil
}

func (c CORS) allowed(origin string) bool {
	return origin != "" && c.origins[strings.ToLower(origin)]
}

// cors answers preflight requests itself (they carry no credentials, so
// they must not reach auth or rate limits) and adds CORS headers to
// responses for allowed origins.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		h := w.Header()
		h.Add("Vary", "Origin")
		if !s.CORS.allowed(origin) {
			next.ServeHTTP(w, r)
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
