package httpx_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"registry-stats/internal/httpx"
)

// FuzzParseRetryAfterRegistry exercises ParseRetryAfter with arbitrary strings.
// Invariants: never panics, result >= 0, result <= 60s cap.
func FuzzParseRetryAfterRegistry(f *testing.F) {
	f.Add("")
	f.Add("0")
	f.Add("5")
	f.Add("120")
	f.Add("-1")
	f.Add("soon")
	f.Add("  10  ")
	f.Add(time.Now().Add(5*time.Minute).UTC().Format(http.TimeFormat))
	f.Add(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))

	f.Fuzz(func(t *testing.T, input string) {
		d := httpx.ParseRetryAfter(input)
		if d < 0 {
			t.Errorf("ParseRetryAfter(%q) = %v, want >= 0", input, d)
		}
		if d > 60*time.Second {
			t.Errorf("ParseRetryAfter(%q) = %v, want <= 60s", input, d)
		}
	})
}

// FuzzRedirectPolicyRegistry exercises RedirectPolicy with arbitrary hostnames.
// Verifies the suffix-bypass concern: a host like "evildocker.com" must NOT be allowed
// just because it ends in "docker.com". Only exact matches or proper dot-prefixed
// suffixes should pass.
func FuzzRedirectPolicyRegistry(f *testing.F) {
	f.Add("hub.docker.com")
	f.Add("auth.docker.com")
	f.Add("github.com")
	f.Add("api.github.com")
	f.Add("raw.githubusercontent.com")
	f.Add("evil.com")
	f.Add("evildocker.com")
	f.Add("notdocker.com")
	f.Add("fakegithub.com")
	f.Add("localhost")
	f.Add("127.0.0.1")
	f.Add("")

	f.Fuzz(func(t *testing.T, host string) {
		u, err := url.Parse("https://" + host + "/path")
		if err != nil {
			return // invalid URL, skip
		}
		req := &http.Request{URL: u}
		policyErr := httpx.RedirectPolicy(req, nil)

		// If the policy allows this host, verify it's actually in the allowlist:
		// exact match or proper dot-prefixed suffix of docker.com, github.com,
		// or githubusercontent.com.
		if policyErr == nil {
			hostname := u.Hostname()
			allowed := hostname == "hub.docker.com" ||
				hostname == "github.com" ||
				(len(hostname) > len(".docker.com") && hostname[len(hostname)-len(".docker.com"):] == ".docker.com") ||
				(len(hostname) > len(".github.com") && hostname[len(hostname)-len(".github.com"):] == ".github.com") ||
				(len(hostname) > len(".githubusercontent.com") && hostname[len(hostname)-len(".githubusercontent.com"):] == ".githubusercontent.com") ||
				// The production code also allows the bare suffixes themselves
				// (e.g. ".docker.com") via HasSuffix — these are not exploitable
				// since they are not routable hostnames.
				hostname == ".docker.com" ||
				hostname == ".github.com" ||
				hostname == ".githubusercontent.com"
			if !allowed {
				t.Errorf("RedirectPolicy allowed host %q which is not in the allowlist", hostname)
			}
		}
	})
}
