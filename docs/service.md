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
and links no database driver, SQL package or broker client. Both rules are
enforced by tests in `cmd/factortrail/policy_test.go`.

## Profiles

The server starts with exactly one explicit profile (`--profile` or
`FACTORTRAIL_PROFILE`). The only profile is `retro`. An empty or unknown name
refuses to start. A profile is a static table in `internal/mcp/profile.go`;
there is no default and no dynamic filtering.

`tools/call` and `tools/list` are checked against the active profile on every
request, so a handler that exists on the server but is not in the profile is
neither listed nor callable. The `retro` profile has no tools yet.

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
| `--dev` | flag only | local testing mode, see below |

The upstream read token for the `retro/v1` API (`RETRO_API_READ_TOKEN`, read
from a file path and never from argv) is not read yet. It is added with the
upstream client and is not required by this build.

## Startup refusals

The process exits with status `2` and a message that names settings, never
their values, when:

- any of the three Access settings is missing in production, or only some of
  them are set (also in `--dev`);
- the team domain is not a bare host name, or an allowlist entry is not an
  email address;
- the profile is empty or unknown;
- `--dev` is combined with a listen address that is not loopback;
- `--public-host` is not a bare host name with an optional port.

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
succeeded. If the key set cannot be refreshed the request is refused: expired
keys are dropped and never used again. RSA keys below 2048 bits are ignored.

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

## Release policy

- CI (`.github/workflows/ci.yml`): job `verify` runs gofmt, `go vet`,
  `go test`, `go test -race`, `go build` and a container build with a smoke
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
