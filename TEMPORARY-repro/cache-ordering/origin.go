package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// etagVaryHeaders is the ordered set of request headers GitHub folds into its ETag digest.
//
// This list and the digest below are deliberately re-implemented here rather than borrowed from
// github-conditional-http-transport: the origin emulator has to be an independent statement of what
// GitHub does, otherwise the repro would be asserting the library against itself.
var etagVaryHeaders = []string{"Accept", "Authorization", "Cookie"}

// originETag reproduces GitHub's ETag derivation: sha256 over each present vary header value
// followed by ":", in a fixed header order, followed by the raw response body. Headers that are
// absent from the request contribute nothing at all -- not even a separator.
func originETag(h http.Header, body []byte) string {
	digest := sha256.New()

	for _, name := range etagVaryHeaders {
		for _, value := range h.Values(name) {
			digest.Write([]byte(value))
			digest.Write([]byte(":"))
		}
	}

	digest.Write(body)

	return hex.EncodeToString(digest.Sum(nil))
}

// etagMatches reports whether an If-None-Match header satisfies the supplied entity tag. GitHub
// accepts both the strong (`"hex"`) and weak (`W/"hex"`) forms, as well as a comma separated list.
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}

	want := strings.Trim(etag, `"`)

	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")

		if strings.Trim(candidate, `"`) == want {
			return true
		}
	}

	return false
}

// observation is a single request as seen by the origin, which is the only vantage point from which
// a conditional-request cache hit is visible. github-conditional-http-transport rewrites a wire 304
// into a 200 before returning it, so counting statuses at the caller shows 200s either way.
type observation struct {
	Status       int
	HadAuth      bool
	Accept       string
	IfNoneMatch  string
	ComputedETag string
}

// origin is an httptest-backed emulation of a GitHub REST endpoint that serves a fixed body and
// implements GitHub's documented conditional-request behaviour.
type origin struct {
	body []byte

	mu           sync.Mutex
	observations []observation
}

func newOrigin(body string) *origin {
	return &origin{body: []byte(body)}
}

func (o *origin) start() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(o.serve))
}

func (o *origin) serve(w http.ResponseWriter, r *http.Request) {
	obs := observation{
		HadAuth:     r.Header.Get("Authorization") != "",
		Accept:      r.Header.Get("Accept"),
		IfNoneMatch: r.Header.Get("If-None-Match"),
	}

	// Reject anonymous requests outright so "the cache stripped the credential" and "the cache sent
	// the wrong validator" are distinguishable failure modes rather than both surfacing as a 200.
	if !obs.HadAuth {
		obs.Status = http.StatusUnauthorized
		o.record(obs)
		http.Error(w, `{"message":"Requires authentication"}`, http.StatusUnauthorized)

		return
	}

	etag := originETag(r.Header, o.body)
	obs.ComputedETag = etag

	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Vary", "Accept, Authorization")
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if etagMatches(obs.IfNoneMatch, etag) {
		obs.Status = http.StatusNotModified
		o.record(obs)
		w.WriteHeader(http.StatusNotModified)

		return
	}

	obs.Status = http.StatusOK
	o.record(obs)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(o.body)
}

func (o *origin) record(obs observation) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.observations = append(o.observations, obs)
}

// snapshot returns a copy of every request the origin has served, and resets nothing.
func (o *origin) snapshot() []observation {
	o.mu.Lock()
	defer o.mu.Unlock()

	out := make([]observation, len(o.observations))
	copy(out, o.observations)

	return out
}

func (o *origin) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.observations = nil
}
