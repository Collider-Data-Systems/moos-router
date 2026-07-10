# AGENTS.md — moos-router (Go federation router)

> **Thin mirror.** Cross-tool brief read natively by Copilot / Cursor / Codex / Gemini / Antigravity; Claude reads it via `CLAUDE.md`. Manual — don't edit except emergency de-rot. Repo-local deltas only; the fleet/project SOT is `ffs0/AGENTS.md`.

## What this repo is
`moos-router` is the **Go federation router** for mo:os — a thin, **stateless** HTTP proxy implementing WF16 (cross-kernel read cascade + URN-routed write-proxy). It is **NOT a kernel**: no rewrite log, no operad, no fold, no §M11/§M12 gates. **Log-is-truth stays at each `moos-kernel` instance**; this binary only fans out reads, routes writes to the kernel-of-record, and cascades to peer routers. Stdlib-only (`go 1.23`, zero deps).

## SOT hierarchy (read first)
```
HG folded state · ontology.json · live /healthz readback  → SEMANTIC SOT (truth; state is derived from the log)
ffs0/AGENTS.md (project SOT) + ffs0 KB                     → authored projection SOT
README.md (this repo) · CLAUDE.md / mirrors               → thin tool-deltas
```
A Markdown file is never the final truth. HG is. **Live runtime truth: `ffs0/kb/superset/running-state.md` + `/healthz` + `GET /admin/topology`.** Federation topology: `ffs0/dev/config/moos-federation.topology.json` — **read by the binary** (`--topology-file`, loaded at boot, hot-reloadable). Launchers: `ffs0/dev/scripts/ops/start_federation_{z440,laptop,hpprodesk}.ps1`.

## The rule (non-negotiable)
Four rewrites only: **ADD · LINK · MUTATE · UNLINK**. **Log is truth. State is derived.** The router never logs, validates, or mutates — it passes envelopes through. Nomenclature — use: node · relation · rewrite · property · operad · port · rewrite_category WF01..WF21 · `_urn`/`_urns`. Never: edge · wire · field · mutation · schema · association · binding · `_ref`.

## Layout
```
cmd/router       — entry point; CLI flag parsing; HTTP serve loop
internal/proxy   — shard map, peer cascade, request routing, fan-out, health
```

## Build / test / branching
```bash
go build ./...    # artifact moos-router.exe is gitignored
go test ./...     # internal/proxy/proxy_test.go — proxy routing only
```
- Default branch `master`. Runtime-code branches `feat/<purpose-slug>` (per `ffs0/AGENTS.md`); `--type-map` type-ID routing landed on master (the old `feat/type-map-routing` worktree note is obsolete).
- Merge-with-provenance: trailer `authored-by: <agent-urn> / <session-urn> / <purpose-slug>`.

## Safety / boundaries
Stateless contract — do **not** add logging of envelope bodies (log-is-truth stays at each kernel). Never commit `secrets/`, credentials, `.vscode/mcp.json`, or the `moos-router.exe` binary. Mutations (commit/push/merge) are explicit boundary acts. Cover new route behavior in `internal/proxy/proxy_test.go`.

---
authored-by: agent:claude-cowork.hp-z440 / session:sam.z440-cowork-workspace / config-overhaul
