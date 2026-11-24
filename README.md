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
| `testkit` | test helpers that never touch a real data directory |

Compatibility with the existing Node servers is tested with vectors generated from the Node implementations (sealed values, cookie signatures, bcrypt hashes, helmet and rate-limit captures).

```bash
go vet ./... && go test -race ./...
```

Licensed under AGPL-3.0, like the apps that use it.
