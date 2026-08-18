# TEMPORARY: the conditional request cache never produced cache hits for authenticated users

> **This document is temporary.** It exists to explain a transport ordering fix and the
> evidence behind it. Delete it, or fold the relevant parts into `ARCHITECTURE.md` / `DECISIONS.md`,
> once the change has been reviewed.

## Symptom

The conditional-request cache transport
(`github.com/bored-engineer/github-conditional-http-transport`, imported as `ghct`) produced a
~0% cache hit rate. Because every real user of the provider is authenticated, the feature was
effectively inert: no `304 Not Modified` responses were ever served from the cache.

**This affected every `legacy_client = false` user, not just those who set `cache_path`.**
`github/provider.go:613` sets `Cache: true` unconditionally for the non-legacy client, and when
`cache_path` is unset the source constructors fall back to `os.MkdirTemp` (`token.go:21`,
`app.go:45`, `anonymous.go:21`). Caching is therefore always on; `cache_path`'s function is
cross-process persistence — choosing where the cache lives so it survives between runs — not
enabling the cache. Users who set it lost that cross-run reuse on top of the in-run loss everyone
else took.

Two bounds on that scope, stated so the claim above is not overread:

- **The non-legacy client is itself opt-in.** `legacy_client` defaults to `true`
  (`github/provider.go:163`, `EnvDefaultFunc("GITHUB_LEGACY_CLIENT", true)`), so the affected
  population is users who explicitly set `legacy_client = false` or `GITHUB_LEGACY_CLIENT=false`,
  not the default configuration. This plausibly explains how a ~0% hit rate went unnoticed: the
  code path is off by default.
- **The cache is REST-only.** `getGraphQLClientOptions`
  (`internal/ghclient/options.go:42`) never sets `Cache`, while `getRESTClientOptions` does.
  GraphQL traffic is unaffected either way, since it is all `POST` and `ghct` declines to cache
  anything other than `GET`/`HEAD`. Benefit estimates should exclude v4 traffic.

## Mechanism

`newTransport` in `internal/ghclient/transport.go` builds the RoundTripper chain inside-out. Before
this change the construction order was:

```go
tr := cloneTransport(http.DefaultTransport, opts)
if tokenSource != nil { tr = &oauth2.Transport{Base: tr, Source: tokenSource} } // innermost
tr = logging.NewLoggingHTTPTransport(tr)
tr = &retryablehttp.RoundTripper{...}
tr = &throttler{...}
tr = ratelimit.New(tr, ...)
if opts.Cache { tr = ghct.NewTransport(store, tr) }                             // outermost
```

A RoundTripper chain executes **outermost-first**: the outermost transport sees the request before
any inner transport has touched it. `oauth2.Transport` attaches the `Authorization` header on the way
down, so every transport above it — including `ghct` — observed a request with no credential.

That matters because GitHub varies its ETags on the credential. GitHub computes an ETag as
`sha256(Accept + ":" + Authorization + ":" + Cookie + ":" + body)` and advertises
`Vary: Accept, Authorization, ...` on the response. The `ghct` library is built specifically around
that behaviour, and all three of its `Authorization`-dependent paths broke:

| path | file (v0.0.7) | behaviour without `Authorization` |
| --- | --- | --- |
| store | `transport.go` (~line 169) | Iterates the response's `Vary` headers and copies matching **request** headers into `X-Varied-*` markers. `req.Header.Values("Authorization")` was empty, so `X-Varied-Authorization` was never written. |
| reuse | `vary.go` (~line 23) | Compares `HashToken(req.Header.Get("Authorization"))` against the stored `X-Varied-Authorization`. `HashToken("")` is a non-empty digest of the empty string, so it never matched the missing stored value — always false. |
| recompute | `hash.go` (~line 29) / `conditional.go` | With the vary check false, the library recomputes the expected ETag over the cached body. It hashed **without** `Authorization` while GitHub hashed **with** it, so the resulting `If-None-Match` was guaranteed to mismatch and GitHub always answered `200`. |

The net effect on every cached request was: store an entry that can never be matched, then send a
wrong `If-None-Match` forever.

> Note: the original bug report referenced the store path as `cht.go`. In v0.0.7 that code lives in
> `transport.go`; the line number and logic are otherwise as described.

The doc comment on `newTransport` already described the intended ordering ("wraps the provided token
source with OAuth2 authentication, adds conditional request caching"), which suggests the ordering
was an accident rather than a deliberate tradeoff.

## Fix

`oauth2.Transport` now sits directly outside `ghct`, and the pair sits inside every layer that can
wait or re-issue a request. Resulting execution order:

```
ratelimit -> throttler -> retry -> oauth2 -> ghct -> logging -> base
```

`ghct` now sees the `Authorization` header, writes the `X-Varied-Authorization` marker, matches it on
reuse, and sends the stored ETag verbatim as `If-None-Match`.

The oauth2 transport's position has to satisfy two independent constraints, and upstream's placement
satisfied only one of them. The first attempt at this fix was a straight move to the outermost
position, which traded one constraint for the other:

| placement | cache sees `Authorization` | every wire attempt gets a fresh token |
| --- | --- | --- |
| innermost (upstream) | no | yes |
| outermost (first attempt) | yes | no |
| between the waiting layers and the cache (current) | yes | yes |

The two constraints pull in opposite directions — the cache needs the credential attached *early*, the
waiting layers need it attached *late* — but they are not actually in conflict, because the cache is
not one of the layers that waits. Putting oauth2 between them satisfies both. See
[Token freshness](#token-freshness-why-oauth2-is-not-the-outermost-layer) for the freshness half.

### Consequence for the debug log

`logging` now sits below `ghct`, so it records wire traffic only — a locally-served cache hit produces
no log entry. That is the correct semantic for a transport whose purpose is dumping the actual bytes
sent, but anyone using `TF_LOG_PROVIDER=DEBUG` output to count API requests is counting **wire
attempts**, not logical requests. In practice the totals barely move between cold and warm runs
because GitHub conditional requests always revalidate; what changes is the `200`/`304` split.

This creates a diagnostic trap that the fix itself introduces, so it is worth stating plainly.
`ghct` reports its own decisions through the `Cache-Status` (RFC 9211) and `X-Cache` response
headers — but it sets them on the response it returns *upward*, after rewriting a `304` into the
cached `200`. Because `logging` sits below `ghct`, those headers do not exist yet when the request
is logged, and the `304` the log records has already been converted by the time anything above sees
it. **So a healthy cache and a dead one look nearly identical in `TF_LOG` output**, and the obvious
debugging instinct — turn on debug logging, observe that the request count did not drop, conclude
the cache is not working — reaches the wrong answer.

To actually observe cache behaviour, inspect the response headers at the call site (harness or
client level) where `Cache-Status`/`X-Cache` are present, or compare the `200`/`304` split rather
than the request total.

## Token freshness: why oauth2 is not the outermost layer

`oauth2.Transport` calls `Source.Token()` once per `RoundTrip`. Placing it outermost stamps the
credential onto the request *before* the rate limiters, the throttler and the retry client have done
their waiting — and those layers re-issue the same `*http.Request`, header included:

- `github_secondary_ratelimit` sleeps for the advertised duration and then recurses on the same
  request (`secondary_rate_limit.go:43-66`).
- The primary limiter waits until the reset time, which can be up to an hour.
- `retryablehttp` replays the request after backoff, and its default policy does **not** retry `401`.

GitHub App installation tokens are handed out with as little as 30 seconds of remaining validity
(`go-githubauth` `DefaultExpirySkew = 30 * time.Second`, `auth.go:36`), so a wait longer than that
would put a dead credential on the wire and surface as an unretried `401`. Personal access tokens use
`oauth2.StaticTokenSource` and were never affected.

Keeping oauth2 inside the waiting layers restores the guarantee the original ordering had by
accident: every attempt that reaches the wire carries a token minted moments earlier.

Moving `ghct` below the rate limiter costs nothing. It rewrites only `304` responses; every other
status passes through untouched (`transport.go:156-193`), so a real `403` still reaches limit
detection intact. Rate limit accounting also stays accurate: the `x-ratelimit-*` headers on a
revalidation come from the fresh `304`, and the limiter observes them on the way back out.

## Evidence

Measured before the change was written, using a standalone Go harness against the live GitHub API.
These numbers are reproduced from that run and were **not** re-measured as part of this commit.

Number of `304` responses observed, per ordering:

| ordering | cold | warm (same auth) | warm (changed `Accept`) |
| --- | --- | --- | --- |
| upstream | 0 | **0** | **0** |
| patched | 0 | **10** | **10** |

The changed-`Accept` column exercises the library's recompute path, which is the same path a rotated
token takes.

End-to-end against a real 8,760-instance Terraform state:

| build | billable API requests (cold) | billable API requests (warm) | wall clock |
| --- | --- | --- | --- |
| upstream | 7,830 | 7,830 | 398s |
| patched | 4,336 (-44.6%) | 1,779 (-77.3%) | 387s |

Separately confirmed against the live API: `304` responses consume zero rate-limit quota, and GitHub
accepts both the strong (`"hex"`) and weak (`W/"hex"`) `If-None-Match` forms.

## Test coverage

Three subtests were added to `Test_newTransport` in `internal/ghclient/transport_test.go`. The first
two run against an `httptest.Server` that mimics GitHub: it requires `Authorization`, advertises
`Vary: Accept, Authorization`, and answers `304` only when the exact ETag it issued is echoed back.

- `transport_caches_authenticated_requests` builds a client through the real `newTransport` with a
  token source and `Cache: true`, issues the same request twice, and asserts the second is a cache
  hit backed by exactly one `304`. Against the previous ordering this fails with
  `expected 1 revalidated (304) request, got 0`.
- `cache_transport_inside_oauth2_never_revalidates` constructs the previous ordering explicitly and
  asserts it never revalidates, pinning the regression.
- `each_retry_attempt_mints_a_fresh_token` uses a token source that returns a distinct token per
  call and a server that fails the first attempt, then asserts the retry carried a different
  credential. With oauth2 outermost it fails with `expected each attempt to carry a freshly minted
  token, got "Bearer token-1" twice`.

The pre-existing `transport_caches_requests` subtest passes under both orderings because it uses no
token source, which is why the bug went unnoticed.

Note that the two regression subtests are characterization tests of third-party behaviour:
`cache_transport_inside_oauth2_never_revalidates` depends on `ghct` continuing to require an
`Authorization` header, and the logging subtest below depends on the SDK continuing to dump headers
verbatim. If either dependency changes, those tests will fail even though the provider is correct.

## Follow-up (fixed): `Authorization` was visible to the logging transport

`oauth2.Transport` used to sit *inside* `logging.NewLoggingHTTPTransport`, so the logging transport
ran first and dumped the request before the credential was attached — redaction by accident. The
reorder puts oauth2 above logging, so the logging transport now sees the fully-authenticated request.

`helper/logging.NewLoggingHTTPTransport` (terraform-plugin-sdk v2.40.1) calls
`httputil.DumpRequestOut(req, true)` at `logging_http_transport.go:201` and turns every header into a
`tflog` field via `fieldHeadersFromRequestReader`. Neither it nor the standard library redacts
anything: on Go 1.26.0, `DumpRequestOut` emits `Authorization` verbatim (verified directly). So with
`TF_LOG_PROVIDER=DEBUG` the reorder would have written the raw token to the provider log.

### What was done

`newTransport` now wraps the logging transport in a `maskingTransport`, which applies
`tflog.MaskFieldValuesWithFieldKeys(req.Context(), "Authorization")` to the request context before
delegating. The wrap happens at the point the logging transport is constructed, so nothing can be
reordered between the two later.

### Why not mask at provider configuration time

`tflog.MaskFieldValuesWithFieldKeys` (terraform-plugin-log v0.11.0, `tflog/provider.go:225`) stores
the masking options in the context it returns; there is no global state. For that mask to apply, the
returned context has to be the one the transport logs against — `req.Context()`.

It cannot be. `helper/schema.ConfigureContextFunc` is
`func(context.Context, *ResourceData) (interface{}, diag.Diagnostics)` (`helper/schema/provider.go:178`)
— no context out-parameter, so a masked context built during `Configure` is unreturnable. And each
CRUD RPC derives its own context from its own incoming gRPC context via `logging.InitContext(ctx)`
(`helper/schema/grpc_provider.go:724`, `:798`, `:1359`), so it would not inherit one even if it could
be stored.

Masking at `Configure` would therefore compile, run, and do nothing — a fix that appears to work while
the credential still leaks. `Test_newTransport/logging_transport_leaks_authorization_without_in_chain_masking`
pins this: it masks one context, logs a control entry through it (asserting that entry *is* masked, so
the sink is provably working), then issues a request on an unmasked context through an unmasked chain
and asserts the token appears verbatim.

### Why not move logging above oauth2

That restores the old redaction-by-accident but loses the transport's stated purpose. Logging would
run before `ghct` adds `If-None-Match`, before retry, and before rate limiting — so it would show
neither conditional requests, nor 304 revalidations, nor retried attempts, and would log
cache-served responses instead of wire responses. Rejected.

### Scope and a note for reviewers

The mask covers the `Authorization` field key only. `DumpRequestOut` puts headers in the dump, but the
SDK consumes them into discrete fields with `ReadMIMEHeader` before the remainder becomes
`tf_http_req_body`, so the body field does not carry the credential. Response fields never do.

An alternative worth considering upstream: apply the mask once at the provider's `tflog` sink instead
of scoped to this chain. That is defence in depth — it would catch any future transport or code path
that logs headers — at the cost of being a provider-wide policy rather than a local guarantee. The
chain-scoped version is implemented here because it is self-contained and testable; the broader
version is a maintainer's call.

## Adjacent issues found, not fixed here

Two pre-existing issues surfaced while validating the `cache_path` story. Both are out of scope for
this change and are recorded only so the persistence advice above is not read as unqualified.

- **A shared `cache_path` serialises concurrent processes.** `createCacheStore`
  (`internal/ghclient/cache.go`) calls `ghctbbolt.Open(path, 0o600, nil, nil)`; the `nil` options
  leave bbolt's `Timeout` at `0`, which means wait for the file lock *indefinitely* rather than
  fail. Two Terraform runs pointed at the same `cache_path` will block one another rather than
  error, and the store is never closed. Anyone sharing a cache directory across parallel CI jobs
  should know this before doing so.
- **Temporary cache directories are never cleaned up.** When `cache_path` is unset, the
  `os.MkdirTemp` fallback leaks one directory per run.

## Note on branch base

This branch is based on `integrations/terraform-provider-github` `main`
(`c55240a2fb1c0dc51090bdbebd4b8bb5aae1c9e0`, 30 commits past `v6.13.0`), not on this fork's `main`,
which is a v5-era commit from August 2023 and does not contain `internal/ghclient` at all.
`internal/ghclient/transport.go` is byte-identical between `v6.13.0` and upstream `main`.
