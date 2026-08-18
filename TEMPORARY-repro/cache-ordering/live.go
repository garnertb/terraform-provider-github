package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// liveEndpoint is a cheap, stable, read-only REST endpoint. It is not a list endpoint, so the
// speculative empty-array ETag guess can never produce a misleading hit.
const liveEndpoint = "https://api.github.com/user"

// runLive exercises the same two chains against the real API. It is secondary to the hermetic mode
// and skips cleanly when no credential is available.
func runLive(ctx context.Context, w io.Writer) error {
	fmt.Fprintln(w, "\n== live mode ==")

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(w, "SKIP: GITHUB_TOKEN is not set, so the live mode has no credential to use.")
		return nil
	}

	tmp, err := os.MkdirTemp("", "cache-ordering-repro-live")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	var results []runResult

	for _, order := range []ordering{orderingUpstream, orderingPatched} {
		result, err := scenario(ctx, order, token, liveEndpoint, filepath.Join(tmp, string(order)), requestsPerScenario)
		if err != nil {
			return fmt.Errorf("%s ordering: %w", order, err)
		}

		results = append(results, result)
	}

	// Against the real API there is no origin-side vantage point, so the wire 304s are only visible
	// through the headers github-conditional-http-transport sets on the way back out.
	fmt.Fprintf(w, "\n%d identical GETs of %s per chain\n", requestsPerScenario, liveEndpoint)
	fmt.Fprintln(w, "(origin statuses are unobservable against the real API; read the X-Cache column)")
	fmt.Fprintln(w)
	report(w, results)

	upstreamHits := countClientCacheHits(results[0].Client)
	patchedHits := countClientCacheHits(results[1].Client)

	fmt.Fprintf(w, "\nlive cache hits: upstream=%d patched=%d (of %d requests)\n",
		upstreamHits, patchedHits, requestsPerScenario)

	if upstreamHits == patchedHits {
		return fmt.Errorf("live: both orderings reported %d hits -- inconclusive, not a finding", upstreamHits)
	}

	return nil
}
