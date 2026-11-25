# neolabs-go

Shared Go foundation of the NeoLabs apps (NeoAgent, NeoRouter, NeoCloud, NeoRecall). One implementation of each protection and building block, used by every app, so the four share the same behaviour and the same security baseline.

| Package | What it gives you |
|---|---|
| `secretbox` | AES-256-GCM sealed secrets at rest (`enc:v1:` layout) |
| `sessionx` | HMAC-signed cookie sessions on a SQLite store, express-session compatible |
| `passhash` | bcrypt hashes, random tokens, SHA-256 token digests, constant-time compare |
| `secheaders` | baseline security headers and an ordered CSP builder |
| `httpx` | JSON helpers, middleware chain, CORS, origin guard, API case guard, panic recovery |
| `ratelimit` | fixed-window limiter with `RateLimit-*` headers |
| `ssrf` | outbound HTTP client that only reaches public addresses |
| `runtimecfg` | data directories (0700), private files (0600), `.env` loading |
| `dbx` | SQLite (pure Go, FTS5), writer/reader pools, `user_version` migrations |
| `filebox` | Versioned AES-256-GCM authenticated whole-file/stream sealing with magic headers |
| `testkit` | test helpers that never touch a real data directory |
| `cli` | app CLI building blocks: `--json` setup event stream, terminal output and prompts, port probes, command list |
| `cli/envfile` | `.env` read/edit with atomic private (0600) writes |
| `cli/service` | per-user service: launchd agent, systemd user unit, Windows logon task, detached fallback |
| `cli/release` | release channels, version order, Ed25519-signed manifests, verified HTTPS downloads, safe zip extraction, tag-based release lookup, and invocation-aware launcher refresh |
| `cli/oauthcli` | command-line OAuth sign-in: PKCE browser flow with a loopback redirect, device-code polling |
| `googleauth` | access tokens from Google Application Default Credentials files (service account JWT or authorized user), no SDK |
| `llm/openaiimages` | OpenAI-compatible `/images/generations` client (OpenAI, xAI) returning base64 images |

Compatibility with the existing Node servers is tested with vectors generated from the Node implementations (sealed values, cookie signatures, bcrypt hashes, helmet and rate-limit captures).

## Shared installers and GitHub Actions

Apps keep their identity, setup configuration, data migrations and release asset build steps in their own repositories. They use `cli/release` for signed runtime updates and launcher replacement, and `scripts/select-release-asset.sh OWNER/REPO stable|beta ASSET_PREFIX` for bootstrap CLI downloads. The selector checks version tags directly so a stale GitHub release-list response does not pin an installer to an older build.

The reusable Go and Flutter setup actions live in `.github/actions/setup-go` and `.github/actions/setup-flutter`. App workflows should reference these actions by an immutable commit SHA.

```bash
go vet ./... && go test -race ./...
```

Licensed under AGPL-3.0, like the apps that use it.
