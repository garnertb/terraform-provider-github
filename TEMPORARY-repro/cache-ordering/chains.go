package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	ghct "github.com/bored-engineer/github-conditional-http-transport"
	ghctbbolt "github.com/bored-engineer/github-conditional-http-transport/bbolt"
	ratelimit "github.com/gofri/go-github-ratelimit/v2/github_ratelimit"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/logging"
	"golang.org/x/oauth2"
)

// ordering identifies which of the two RoundTripper construction orders to build.
type ordering string

const (
	// orderingUpstream is the chain as built by internal/ghclient.newTransport on main: the
	// conditional cache is the outermost layer and oauth2 is the innermost, so the cache runs
	// before the Authorization header exists.
	orderingUpstream ordering = "upstream"

	// orderingPatched places oauth2 directly outside the conditional cache, so the cache observes
	// the Authorization header that GitHub varies its ETags on.
	orderingPatched ordering = "patched"
)

// newBaseTransport mirrors ghclient.cloneTransport.
func newBaseTransport() http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true

	return tr
}

// withRetries mirrors the retryablehttp layer in ghclient.newTransport, with waits shortened so the
// repro stays fast.
func withRetries(inner http.RoundTripper) http.RoundTripper {
	client := retryablehttp.NewClient()
	client.Logger = nil
	client.HTTPClient = &http.Client{Transport: inner, Timeout: 30 * time.Second}
	client.RetryMax = 3
	client.RetryWaitMin = time.Millisecond
	client.RetryWaitMax = 10 * time.Millisecond

	return &retryablehttp.RoundTripper{Client: client}
}

// buildChain assembles one of the two transport chains from the same libraries, and at the same
// pinned versions, that the provider uses. It deliberately does not import internal/ghclient: the
// ordering bug is a property of these libraries composed in this order, and nothing else.
func buildChain(order ordering, tokenSource oauth2.TokenSource, store ghct.Storage) http.RoundTripper {
	tr := newBaseTransport()

	switch order {
	case orderingUpstream:
		if tokenSource != nil {
			tr = &oauth2.Transport{Base: tr, Source: tokenSource}
		}

		tr = logging.NewLoggingHTTPTransport(tr)
		tr = withRetries(tr)
		tr = ratelimit.New(tr)
		tr = ghct.NewTransport(store, tr)

	case orderingPatched:
		tr = logging.NewLoggingHTTPTransport(tr)
		tr = ghct.NewTransport(store, tr)

		if tokenSource != nil {
			tr = &oauth2.Transport{Base: tr, Source: tokenSource}
		}

		tr = withRetries(tr)
		tr = ratelimit.New(tr)

	default:
		panic("unknown ordering: " + string(order))
	}

	return tr
}

// openStore creates a bbolt-backed ghct store under dir, matching ghclient.createCacheStore.
func openStore(dir string) (*ghctbbolt.Storage, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	store, err := ghctbbolt.Open(filepath.Join(dir, "cache.db"), 0o600, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to open cache storage: %w", err)
	}

	return store, nil
}
