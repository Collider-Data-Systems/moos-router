# moos-router

WF16 federation router for mo:os kernels.

Current branch context: `feat/type-map-routing` is the local active branch. Default branch is `master`. Keep changes small and routing-focused.

---

## Repository Role

`moos-router` is a stateless HTTP routing layer for sovereign kernel logs. It does not own HG truth and does not validate envelopes. It decides which kernel or peer router receives a request.

Current routing surface:

- URN-prefix shard routing.
- Type-ID routing via `--type-map`, checked before shard rules for writes.
- Peer cascade for `GET /state/nodes/{urn}`.
- Fan-out/merge for broad read paths.
- Router health at `/healthz`.

---

## Project Boundary

The runtime stack is split:

- `moos-kernel`: OS-facing runtime function program, log/fold/operad/session/authority/transport.
- `moos-router`: federation routing across kernels.
- `ffs0`: ontology, skills, projection planners, reports, and local dashboards.
- Application groups such as `my-tiny-data-collider`: HG domains that may use websites, DNS, servers, Calendar, GitHub, and Workspace surfaces through the kernels.

Do not put application-specific website/DNS/domain logic in this repo unless it is explicitly router configuration or a generic federation feature.

---

## Development

Run tests with:

```powershell
go test ./...
```

Primary files:

- `cmd/router/main.go` — CLI flags and router construction.
- `internal/proxy/proxy.go` — routing, proxying, health, fan-out, peer cascade.
- `internal/proxy/proxy_test.go` — expected behavior.

Keep dependencies minimal. Prefer stdlib.

---

## Safety

- No secrets in this repo.
- Do not commit generated binaries.
- Do not rewrite Git history or reset user work.
- When adding new route behavior, cover it in `internal/proxy/proxy_test.go`.
