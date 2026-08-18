package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	ghct "github.com/bored-engineer/github-conditional-http-transport"
	"golang.org/x/oauth2"
)

const acceptHeader = "application/vnd.github+json"

// clientObservation is a response as seen by the caller, above the cache.
type clientObservation struct {
	Status      int
	XCache      string
	CacheStatus string
}

// runResult is everything one scenario produced, from both vantage points.
type runResult struct {
	Ordering ordering
	Client   []clientObservation
	Origin   []observation

	// StoredVariedAuthorization is the X-Varied-Authorization marker found in the cache entry after
	// the run, or "" when the marker was never written. This is the direct observation of the first
	// of the three broken code paths.
	StoredVariedAuthorization string
	StoredEntryFound          bool
}

// run issues n identical GET requests through chain and collects both client-side and
// origin-side observations.
func run(ctx context.Context, chain http.RoundTripper, url string, n int) ([]clientObservation, error) {
	client := &http.Client{Transport: chain}
	out := make([]clientObservation, 0, n)

	for i := range n {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("request %d: %w", i, err)
		}

		req.Header.Set("Accept", acceptHeader)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request %d: %w", i, err)
		}

		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			resp.Body.Close()

			return nil, fmt.Errorf("request %d: draining body: %w", i, err)
		}

		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("request %d: closing body: %w", i, err)
		}

		out = append(out, clientObservation{
			Status:      resp.StatusCode,
			XCache:      resp.Header.Get("X-Cache"),
			CacheStatus: resp.Header.Get("Cache-Status"),
		})
	}

	return out, nil
}

// inspectStore reads back the cache entry for url and reports whether the vary marker for the
// Authorization header was written.
func inspectStore(ctx context.Context, store ghct.Storage, url string) (found bool, variedAuthorization string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "", err
	}

	cached, err := store.Get(ctx, req)
	if err != nil {
		return false, "", fmt.Errorf("(Storage).Get failed: %w", err)
	}

	if cached == nil {
		return false, "", nil
	}

	defer func() {
		if cached.Body != nil {
			_, _ = io.Copy(io.Discard, cached.Body)
			_ = cached.Body.Close()
		}
	}()

	return true, cached.Header.Get(ghct.VaryPrefix + "Authorization"), nil
}

// scenario builds one chain, drives n requests through it, and inspects the resulting cache entry.
func scenario(ctx context.Context, order ordering, token, url, cacheDir string, n int) (runResult, error) {
	store, err := openStore(cacheDir)
	if err != nil {
		return runResult{}, err
	}
	defer store.DB.Close()

	source := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, TokenType: "Bearer"})

	clientObs, err := run(ctx, buildChain(order, source, store), url, n)
	if err != nil {
		return runResult{}, err
	}

	found, varied, err := inspectStore(ctx, store, url)
	if err != nil {
		return runResult{}, err
	}

	return runResult{
		Ordering:                  order,
		Client:                    clientObs,
		StoredEntryFound:          found,
		StoredVariedAuthorization: varied,
	}, nil
}

func countStatuses(obs []observation) map[int]int {
	out := map[int]int{}
	for _, o := range obs {
		out[o.Status]++
	}

	return out
}

func countClientCacheHits(obs []clientObservation) int {
	hits := 0

	for _, o := range obs {
		if o.XCache == "HIT" {
			hits++
		}
	}

	return hits
}

func formatStatuses(counts map[int]int) string {
	if len(counts) == 0 {
		return "none"
	}

	codes := make([]int, 0, len(counts))
	for code := range counts {
		codes = append(codes, code)
	}

	sort.Ints(codes)

	parts := make([]string, 0, len(codes))
	for _, code := range codes {
		parts = append(parts, fmt.Sprintf("%d x%d", code, counts[code]))
	}

	return strings.Join(parts, ", ")
}

// report renders the comparison table.
func report(w io.Writer, results []runResult) {
	fmt.Fprintf(w, "%-10s  %-22s  %-18s  %-11s  %s\n",
		"ORDERING", "ORIGIN STATUSES", "CLIENT STATUSES", "X-Cache:HIT", "STORED X-Varied-Authorization")
	fmt.Fprintln(w, strings.Repeat("-", 110))

	for _, r := range results {
		stored := "(entry absent)"

		if r.StoredEntryFound {
			stored = "(marker absent)"
			if r.StoredVariedAuthorization != "" {
				stored = fmt.Sprintf("present, len=%d", len(r.StoredVariedAuthorization))
			}
		}

		clientCounts := map[int]int{}
		for _, o := range r.Client {
			clientCounts[o.Status]++
		}

		fmt.Fprintf(w, "%-10s  %-22s  %-18s  %-11d  %s\n",
			r.Ordering,
			formatStatuses(countStatuses(r.Origin)),
			formatStatuses(clientCounts),
			countClientCacheHits(r.Client),
			stored)
	}
}
