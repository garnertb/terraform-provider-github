# Repro: transport ordering defeats the HTTP response cache

**Temporary.** This directory exists to demonstrate a bug. Delete it once the bug is resolved.

## Claim

In `internal/ghclient/transport.go`, `newTransport` builds the RoundTripper chain so that the
conditional-request cache (`github.com/bored-engineer/github-conditional-http-transport`, "ghct") is
the **outermost** layer and `oauth2.Transport` is the **innermost**:

```
cloneTransport -> oauth2 -> logging -> retryablehttp -> throttler -> ratelimit -> ghct
                  ^ innermost                                                     ^ outermost
```

RoundTripper chains execute outermost-first, so ghct processes every request *before* the
`Authorization` header exists. GitHub derives its ETags from the request's `Authorization` header and
advertises `Vary: Accept, Authorization`; ghct exists specifically to handle that. With the
credential invisible to it, ghct writes no `X-Varied-Authorization` marker, its vary check can never
match, and its ETag recomputation is guaranteed to differ from GitHub's.

Result: a cache hit rate of ~0% for authenticated users.

Note the credential is **not** stripped from the wire. `oauth2` still authenticates; it just does so
below the cache. This repro asserts that explicitly, because "the cache broke auth" is the wrong
diagnosis and would send a maintainer down the wrong path.

## Running it

```sh
export PATH="/opt/homebrew/bin:$PATH"   # macOS/Homebrew only; needed if go is not on PATH
export GOTOOLCHAIN=auto                 # go.mod requires a recent toolchain

# Hermetic: no credentials, no network. This is the primary deliverable.
go run ./TEMPORARY-repro/cache-ordering

# Also exercise the real api.github.com.
GITHUB_TOKEN=$(gh auth token) go run ./TEMPORARY-repro/cache-ordering -live
```

The program exits non-zero if any assertion fails.

## What it builds

Three chains, four identical `GET`s through each:

| Chain | Built from | Ordering |
|---|---|---|
| `upstream` | the libraries directly, no provider code | ghct outermost, oauth2 innermost (as on `main`) |
| `patched` | the libraries directly, no provider code | oauth2 directly outside ghct |
| `provider` | `internal/ghclient.NewTokenRESTClient` | whatever the provider actually ships |

The `upstream` and `patched` chains deliberately **do not import `internal/ghclient`**. The bug is a
property of these libraries composed in this order and nothing else, so the core comparison can be
audited without trusting anything in this repository. The `provider` chain is a separate, additional
scenario that ties the result back to the shipped code.

Every chain uses a real `oauth2.StaticTokenSource` and a real bbolt-backed ghct store in a temp dir.

## The emulated origin (hermetic mode)

`origin.go` stands up an `httptest.Server` that emulates GitHub's documented behaviour:

- ETag is `sha256` over the request's `Accept`, `Authorization` and `Cookie` values (in that fixed
  order, each followed by `:`) then the response body, hex-encoded.
- Sends `Vary: Accept, Authorization` and `Cache-Control: private, max-age=60`.
- Returns `304` when `If-None-Match` matches, accepting both `"hex"` and `W/"hex"`.
- Returns `401` when there is no `Authorization` header, so "no credential" and "wrong validator" are
  never confusable.
- Records every request it serves.

**Counting happens at the origin, not the client.** ghct rewrites a wire `304` into a `200` before
returning it, so client-side status codes are `200` in every column and prove nothing on their own.
The client-side signal is ghct's `X-Cache` / `Cache-Status` headers.

## Guarding against a vacuous proof

The failure mode to worry about is producing the expected numbers for the wrong reason. Five
assertions guard that, and all of them are hard failures:

1. **The origin actually works.** Before any chain is measured, a hand-rolled request with an
   independently computed `If-None-Match` must return `304`, in both strong and weak form; the
   served ETag must equal the independent computation; and an unauthenticated request must `401`.
2. **The negative is negative for the right reason.** Under `upstream` ordering the origin must have
   received all four requests, *each carrying a credential* (so the failure is not a stripped
   header), *each carrying a non-empty `If-None-Match`* (so the cache is genuinely in the chain and
   not bypassed), and *none of them matching* (so the validator is wrong, not absent). The cache
   entry must exist — proving the store path is writable and was written — while lacking the
   `X-Varied-Authorization` marker.
3. **The positive is positive.** `patched` must produce exactly three origin `304`s, the client must
   report the same number of `X-Cache: HIT` responses, every client status must be `200`, and the
   stored entry must carry an `X-Varied-Authorization` marker.
4. **The orderings differ.** Identical results across both chains is reported as a harness fault,
   not as a finding.
5. **The provider matches `upstream`.** Zero hits, zero origin `304`s, entry stored, marker absent.

Guard 2 is the load-bearing one. Guards 2 and 5 are what would fail if the fix were reverted.

## Observed output

Hermetic, no credentials:

```
== hermetic mode ==
guard 1 OK: origin returns 304 for a correctly computed If-None-Match, and 401 when unauthenticated

4 identical GETs per chain against the emulated origin

ORDERING    ORIGIN STATUSES         CLIENT STATUSES     X-Cache:HIT  STORED X-Varied-Authorization
--------------------------------------------------------------------------------------------------------------
upstream    200 x4                  200 x4              0            (marker absent)
patched     200 x1, 304 x3          200 x4              3            present, len=44
provider    200 x4                  200 x4              0            (marker absent)

guard 2 OK: upstream ordering reached the origin every time, with a credential and a
            non-empty but incorrect If-None-Match -- the cache is wired up and is sending a
            validator that cannot match, rather than being bypassed
guard 3 OK: patched ordering revalidated (origin 304s, client X-Cache: HIT)
guard 4 OK: the two orderings produced materially different results
guard 5 OK: the provider's own client reproduces the upstream ordering's behaviour
```

Live, against `https://api.github.com/user`:

```
== live mode ==

4 identical GETs of https://api.github.com/user per chain
(origin statuses are unobservable against the real API; read the X-Cache column)

ORDERING    ORIGIN STATUSES         CLIENT STATUSES     X-Cache:HIT  STORED X-Varied-Authorization
--------------------------------------------------------------------------------------------------------------
upstream    none                    200 x4              0            (marker absent)
patched     none                    200 x4              3            present, len=44

live cache hits: upstream=0 patched=3 (of 4 requests)
```

## What this does not prove

- **It does not measure blast radius.** `Cache: true` is set unconditionally for the non-legacy
  client and `cache_path` only chooses where the cache lives, not whether it exists — but
  `legacy_client` defaults to `true`, so the affected population is users who explicitly set
  `legacy_client = false`. That is the likely reason a ~0% hit rate went unnoticed. The cache is
  REST-only; `getGraphQLClientOptions` never sets `Cache`.
- **It does not prove credential rotation breaks caching.** It does not, and any claim that it does
  is wrong. `addConditionalHeaders` has three branches, not two: no entry (speculative `[]` guess),
  entry with a matching vary marker (reuse the stored ETag), and entry with a *non-matching* marker
  (**recompute** the expected ETag from current headers plus the cached body). A failed vary check is
  the slower path, not a miss. It works correctly once `Authorization` is visible.
- **It does not prove the hermetic origin's ETag rule is byte-for-byte GitHub's.** See the caveat
  below. The live mode is what removes that dependency.

## Caveat on the ETag formula

The formula is often written as `sha256(Accept + ":" + Authorization + ":" + Cookie + ":" + body)`.
Read literally, that emits a separator for a header that is absent. ghct does not do that — `hash.go`
iterates `requestHeaders.Values(name)` and contributes nothing at all for a header that is not
present. This origin follows ghct's semantics.

For these requests the two readings coincide (`Accept` and `Authorization` are always present,
`Cookie` never is), so it does not affect the result. But it does mean the hermetic mode shares one
assumption with the library it is testing. The live mode does not: it runs against real GitHub and
reproduces the same 0-vs-3 split with no formula of ours involved.
