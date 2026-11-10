# Service, authentication and release policy

This page describes how `factortrail serve` starts, who may call it, and how
changes reach production. It holds names only: no hostnames, addresses,
accounts or secrets.

## Commands

```text
factortrail serve    run the MCP server
factortrail version  print the build revision
```

`version` prints the revision set at build time (`-ldflags "-X main.revision=<sha>"`,
passed to the image as the `REVISION` build arg). Without it, the VCS revision
that `go build` records is used, and `unknown` when neither exists.

## What `serve` exposes

| Route | Authentication | Purpose |
| --- | --- | --- |
| `/mcp` | Cloudflare Access assertion | MCP over stateless Streamable HTTP, server name `Factortrail` |
| `/healthz` | none | `GET` returns `200` with an empty body |

Every other path is `404`. The process binds `127.0.0.1:8820` unless told
otherwise. It makes no outbound call except the Access key-set (JWKS) fetch,
the calls of the auto_trader provider (see [Retro tools](#retro-tools)) and,
only when a Sentry DSN is set, error reports to that DSN's host (see
[Observability](#observability)). It links no database driver, SQL package or
broker client. Both rules are enforced by tests in
`cmd/factortrail/policy_test.go`.

## Profiles

The server starts with exactly one explicit profile (`--profile` or
`FACTORTRAIL_PROFILE`). The only profile is `retro`. An empty or unknown name
refuses to start. A profile is a static table in `internal/mcp/profile.go`;
there is no default and no dynamic filtering.

`tools/call` and `tools/list` are checked against the active profile on every
request, so a handler that exists on the server but is not in the profile is
neither listed nor callable. The `retro` profile has exactly the eight tools
listed under [Retro tools](#retro-tools) and no other.

Adding a tool is one row in the profile's table plus its handler.

## Configuration

A flag overrides its environment variable. Values never live in Git.

| Name | Kind | Meaning |
| --- | --- | --- |
| `--profile` / `FACTORTRAIL_PROFILE` | required | tool profile to serve |
| `--addr` / `FACTORTRAIL_ADDR` | optional | listen address, default `127.0.0.1:8820` |
| `--public-host` / `FACTORTRAIL_PUBLIC_HOST` | optional | the one trusted public `Host` value behind a reverse proxy |
| `FACTORTRAIL_ACCESS_TEAM_DOMAIN` | env only | Access team domain (bare host name) |
| `FACTORTRAIL_ACCESS_AUD` | env only | Access application audience tag |
| `FACTORTRAIL_ACCESS_ALLOWED_EMAILS` | env only | comma-separated operator email allowlist |
| `FACTORTRAIL_RETRO_BASE_URL` | env only | base URL of the auto_trader `retro/v1` API (`http` or `https`, host only, no user info, query or fragment) |
| `FACTORTRAIL_RETRO_TOKEN_FILE` | env only | path of the file that holds the upstream read token |
| `FACTORTRAIL_METRICS_ADDR` | env only | optional loopback `IP:PORT` of the metrics listener, see [Observability](#observability) |
| `FACTORTRAIL_SENTRY_DSN` | env only | optional Sentry DSN; error reporting is off when unset |
| `FACTORTRAIL_SENTRY_ENVIRONMENT` | env only | optional Sentry environment name |
| `--dev` | flag only | local testing mode, see below |

The two upstream settings go together: both set, or both unset. With neither,
the process starts, the eight retro tools stay listed, and each call answers
that the upstream is not configured.

The upstream read token (`RETRO_API_READ_TOKEN` on the auto_trader side) is
read from the file named by `FACTORTRAIL_RETRO_TOKEN_FILE`, once, when the
process starts. It is never taken from a flag, argv or an environment
variable, and never baked into the image or a build argument. The file holds
one token of printable ASCII (a trailing newline is ignored) and is at most
4 KiB. A token rotation is a new file content plus a restart.

## Startup refusals

The process exits with status `2` and a message that names settings, never
their values, when:

- any of the three Access settings is missing in production, or only some of
  them are set (also in `--dev`);
- the team domain is not a bare host name, or an allowlist entry is not an
  email address;
- the profile is empty or unknown;
- `--dev` is combined with a listen address that is not loopback;
- `--public-host` is not a bare host name with an optional port;
- only one of `FACTORTRAIL_RETRO_BASE_URL` and `FACTORTRAIL_RETRO_TOKEN_FILE`
  is set, the base URL is not an `http` or `https` URL with a host (or carries
  user info, a query or a fragment), or the token file cannot be read, is empty,
  is larger than 4 KiB or does not hold a single printable ASCII token;
- `FACTORTRAIL_METRICS_ADDR` is set but is not an IP literal that is loopback
  with a port (`localhost`, an empty host, `0.0.0.0` and any non-loopback
  address are refused);
- `FACTORTRAIL_SENTRY_DSN` is set but does not parse as a Sentry DSN. The
  message names the setting, never the value.

## Authentication

Authentication runs before the SDK handler sees a request, in
`internal/auth`. `Verifier.Require` wraps any `http.Handler`, so a future
route uses the same gate.

1. The request must carry exactly one `Cf-Access-Jwt-Assertion` header.
   Anything else, including a request with only an `Authorization: Bearer`
   header, is `401`. Inbound bearer tokens do not exist in P0.
2. The assertion must be a JWT signed with RS256 (any other `alg`, including
   `none` and `HS256`, is refused), with issuer `https://<team domain>`, the
   configured audience, and valid `exp`, `nbf` and `iat` with 30 seconds of
   clock skew. `exp` is required.
3. The identity is the verified `email` claim, matched exactly (ASCII
   case-insensitive) against the allowlist. A plain email header is never an
   identity. An assertion without an email claim, such as a service-token
   assertion, is not an operator and gets `403`, as does an email that is not
   on the allowlist.

An assertion that is present must verify. There is no fallback to another
credential after a bad assertion. When a valid assertion is accompanied by a
bearer header, the assertion decides and the `Authorization` header is removed
before the request moves on.

Failures answer `401` or `403` with a body that carries no detail
(`{"error":"unauthorized"}` or `{"error":"forbidden"}`).

### Key set

The signing keys come from `https://<team domain>/cdn-cgi/access/certs`,
fetched with a 3 second timeout, no redirects and a 1 MiB limit, and cached
for 10 minutes. A fetch happens when the cache is stale or a key id is
unknown, at most once every 30 seconds, whether or not the last attempt
succeeded. The refresh runs on its own timeout, not on the request that
triggered it, so a client that disconnects cannot break it for others. A
failed refresh refuses the request that needed it (an unknown key id) and
leaves keys that are still inside their TTL in use. Keys past their TTL are
never used: if they cannot be refreshed, requests are refused. RSA keys below
2048 bits are ignored.

## Host and Origin

The SDK refuses a request that reaches a loopback listener with a non-loopback
`Host` header (DNS rebinding protection). A reverse proxy on loopback forwards
the public `Host`, so that guard would block legitimate traffic.

- Without `FACTORTRAIL_PUBLIC_HOST`, the SDK guard stays on. Run behind a
  proxy only after setting it.
- With `FACTORTRAIL_PUBLIC_HOST=<host>`, the SDK guard is turned off and the
  handler admits exactly that `Host` (ASCII case-insensitive) and refuses every
  other value with `403`. A port is part of the value if it is set.
- Cross-origin browser requests (`Sec-Fetch-Site` other than `same-origin`,
  or a foreign `Origin`) are refused with `403` in both cases. Clients that
  send neither header, such as MCP clients, are unaffected.

## Dev mode

`--dev` is for local tests only. It is a flag, never an environment setting.
It refuses any listen address that is not `localhost` or a loopback IP
literal. Access settings are optional in dev mode: with none set, `/mcp` is
unauthenticated and the process logs a warning; with all three set they are
enforced as in production; a partial set is still refused.

## Retro tools

The `retro` profile serves eight read-only tools. A tool's name is the
`operationId` of the pinned contract, its inputs are exactly the query
parameters of that operation (same names, required set, types, bounds, `tf`
enum and cursor pattern), and it calls that one operation once.

| Tool | Operation |
| --- | --- |
| `retro_day` | `GET /trading/api/retro/v1/retro_day` |
| `retro_symbol` | `GET /trading/api/retro/v1/retro_symbol` |
| `retro_funnel` | `GET /trading/api/retro/v1/retro_funnel` |
| `retro_scoreboard` | `GET /trading/api/retro/v1/retro_scoreboard` |
| `retro_positions` | `GET /trading/api/retro/v1/retro_positions` |
| `mock_experiments` | `GET /trading/api/retro/v1/mock_experiments` |
| `get_bars` | `GET /trading/api/retro/v1/get_bars` |
| `data_gaps` | `GET /trading/api/retro/v1/data_gaps` |

Input schemas are closed: an argument that is not a contract parameter (a
URL, a path, an operation name) is refused before any upstream request. The
upstream response is returned as the tool's structured content, and as its
text content, without change: a decimal string such as `"1.2300"` stays a
string, an integer above 2^53 is not rounded, a trailing zero in a JSON number
is kept and `null` stays `null`.

### Provider

`internal/providers/autotrader` is the only package that calls the upstream.
It does not import the MCP server, so a CI job can run the pinned client
against another provider. Its client is generated from the pinned contract;
the hand-written layer around it:

- takes the base URL from configuration only, and sends the token only as the
  `X-Retro-Read-Token` header, on every call;
- gives each call a timeout (15 seconds by default) and honours cancellation;
- does not follow redirects (a `3xx` is a failure and the redirect target never
  sees the token) and refuses a response body over 4 MiB;
- maps an upstream failure to a contract error code with the contract's fixed
  message (`401` to `auth_missing` or `auth_invalid`, `403` to `scope_denied`,
  `404` to `feature_off`, `422` to `validation_error`, `503` to `timeout`,
  `upstream_unavailable` or `schema_mismatch`; a transport failure, an
  oversize body, a redirect or any other status to `upstream_unavailable`; a
  `200` body that is not one JSON object to `schema_mismatch`). The upstream
  body is read only to choose among the codes the contract allows for that
  status and is never copied into an error, a log line or a tool result.

### Pinned contract

auto_trader owns the HTTP contract (`contracts/retro/v1/openapi.yaml` there).
This repository keeps a pinned copy in `contracts/autotrader/retro/v1/`, with
its synthetic examples, and `contracts.lock.json` records the source
repository, source commit, source paths and the sha256 of each file.
`TestPinnedContractMatchesLock` fails when any pinned file differs from the
lock, or a file is added to or removed from the examples directory.

The generated client `internal/providers/autotrader/client.gen.go` is
committed. The `verify` job regenerates it and fails when the result differs
from the committed file. The generator is `oapi-codegen` v2.8.0, pinned as a
Go `tool` dependency in `go.mod` and run by the `go:generate` line in
`internal/providers/autotrader/generate.go`. The generated response models
do not carry the envelope fields of the spec's `allOf` responses; the provider
does not use them and returns the raw JSON instead.

### Updating the pinned contract

Do this for every new auto_trader commit this service should follow. It never
happens by following auto_trader's main branch.

1. In an auto_trader checkout at the new commit `<sha>`, copy
   `contracts/retro/v1/openapi.yaml` over
   `contracts/autotrader/retro/v1/openapi.yaml`, and
   `contracts/retro/v1/examples/*.yaml` into
   `contracts/autotrader/retro/v1/examples/` (remove files that no longer
   exist there).
2. Edit `contracts.lock.json`: set `source_commit` to `<sha>` and refresh the
   `sha256` of the spec and of every example (`sha256sum <file>`), adding or
   removing example entries to match.
3. Run `go generate ./internal/providers/autotrader` and commit the changed
   `client.gen.go`.
4. Run `go test ./internal/providers/... ./internal/mcp/... ./cmd/factortrail/...`.
   A new, renamed or removed operation or parameter fails the profile and
   input-schema tests until the tools follow the contract.

Only synthetic data may enter the pinned copy. Nothing derived from real
trades, accounts or symbols belongs in this repository.

## Observability

Three outputs, each optional except the logs. None of them carries a request
header, token, cookie, query string, email address or JWT.

### Logs

Logs are JSON, one object per line, on standard output (`slog` JSON handler).
Event names and existing fields are kept from the text form: `serve.start`,
`serve.stop`, `auth.rejected` and the SDK's own lines. `serve.start` adds
`metrics_addr` (the bound loopback address, empty when the metrics listener is
off) and `sentry` (whether reporting is on).

### Metrics

With `FACTORTRAIL_METRICS_ADDR=<loopback IP>:<port>` a second listener serves
`GET /metrics` in Prometheus text format and nothing else. Without it no such
listener is opened, and `/metrics` on the main listener is `404`. The metrics
listener has no authentication, so the address must be a loopback IP literal:
`127.0.0.1` or `::1`, never `localhost`, `0.0.0.0` or an empty host.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `factortrail_http_requests_total` | `route`, `class` | requests on the main listener |
| `factortrail_http_request_duration_seconds` | `route` | time to serve a request |
| `factortrail_mcp_tool_calls_total` | `tool`, `result` | calls of the profile's tools; `result` is `ok` or `error` |
| `factortrail_mcp_tool_duration_seconds` | `tool` | time of a tool call |
| `factortrail_upstream_requests_total` | `operation`, `outcome` | calls to auto_trader; `outcome` is `ok` or the contract error code |
| `factortrail_upstream_request_duration_seconds` | `operation` | time of an upstream call |

Label values come from fixed sets: `route` is `/mcp`, `/healthz` or `other`;
`class` is the status class (`2xx`, `4xx`, ...); `tool` and `operation` are the
eight retro tools or `other`; `outcome` is `ok`, one of the eight contract
error codes or `other`. No path, token, email or user input is a label. The
standard Go and process collectors are not registered.

### Error reporting (Sentry)

Reporting is on only when `FACTORTRAIL_SENTRY_DSN` is set. When it is unset,
no Sentry client is created and nothing is sent, even if the environment
carries `SENTRY_DSN`. The release is the build revision (the same value
`factortrail version` prints). Traces are off (sample rate `0`), and the SDK
does not send default personal data.

Reported events:

- a panic in a request handler (the client gets `500` with no detail);
- a `5xx` response on the main listener, tagged with its fixed route;
- an upstream failure, tagged with the operation and the contract code, except
  `validation_error` and `feature_off`, which are counted in the metrics but
  not reported.

Before an event leaves the process, `BeforeSend` removes:

- the request headers `Authorization`, `Cookie`, `Set-Cookie`,
  `Cf-Access-Jwt-Assertion` and `X-Retro-Read-Token`; every other header is kept
  only after its name and value pass the text rule below; and the request
  environment, cookies, query string, body and URL;
- the user identity and the server name;
- breadcrumbs and extra data.

It then replaces, in every message, exception value and tag, each JWT, each
email address and each configured literal (the token file path, the upstream
base URL, the public host, the Access team domain and audience, and the
allowlisted emails) with `[redacted]`.

### Privacy rule

Nothing that identifies a person, a host or a credential belongs in this
repository, in a test fixture or in a log line. Tests use synthetic values
only, and they assert that those values are absent from the scraped metrics,
the event JSON and the log output.

### Out of scope for this change

- creating the Sentry project and the DSN (operator);
- the NCP scrape configuration (prom-agent, alloy) and log collection;
- deployment (hk 3820).

## Release policy

- CI (`.github/workflows/ci.yml`): job `verify` runs gofmt, `go vet`, a
  regeneration check of the provider client (`go generate`, then fail on any
  difference), `go test`, `go test -race`, `go build` and a container build with a smoke
  check of the revision label; job `secrets` runs a secret scan. Both are
  required checks.
- Image: `ghcr.io/mgh3326/factortrail:sha-<commit>`, built by
  `.github/workflows/publish.yml` for commits on `main` only, with the GitHub
  token and nothing else. The image is static, runs as a non-root user and
  carries the `org.opencontainers.image.revision` label.
- Review and approval: `.mergewake/policy.yaml` marks the auth, MCP profile
  and tool surface, upstream, entrypoint, config, deploy, image, workflow,
  policy and dependency paths as blocking, and requires deploy approval for
  auth, profile and tool surface, configuration and deploy paths.
- Deployment is not part of this repository yet.

## Provenance

`internal/auth/auth.go` adapts the JWKS parsing and the shape of the key cache
from `github.com/mgh3326/handoffkeep` at commit
`4f45717f5cf451dc5b6e535585164c073b0803f8`
(`internal/cfaccess/cfaccess.go`, MIT, Copyright (c) 2026 Kwanghyun Moon).
