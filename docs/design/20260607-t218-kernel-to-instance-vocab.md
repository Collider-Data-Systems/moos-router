# Runtime lane — `kernel` → `instance` vocab migration, router side (T=218)

> **Status: BRANCH ANCHOR / intent note. No code rename in this commit.**
> This branch (`feat/manifold-instances-vocab`) is the router-side authoring lane for the 4.0
> `kernel→instance` rename (delta **D6**). Authored by the Z440 VS Code lead
> (`agent:vscode.hp-z440.primary` / `session:sam.z440-vscode-projection-lead`), 2026-06-07.
> Glued to `ffs0` branch `z440-vscode-lead/manifold-instances-vocab` and its design doc
> `dev/design/20260607-t218-manifold-instance-vocab-delta.md` via the shared purpose-slug
> **`manifold-instances-vocab`**.

## Why this branch exists

`moos-router` is WF16 federation routing: it fans requests out to peer instances and fans state back in.
The `kernel → instance` rename (D6) therefore touches the router's peer vocabulary even though the router
holds no log of its own. Per T218 this runtime work lands on `feat/<purpose-slug>` and merges only when
the build gate passes. This lane keeps the router rename aligned with the kernel rename under one shared
purpose-slug so the manifold can glue both runtime repos plus the `ffs0` design lane.

## Runtime footprint to migrate (survey — NOT yet changed)

- `internal/proxy/` peer/fanout types and field names that say `kernel`.
- `/healthz` fanout payload keys describing peers as "kernels".
- `cmd/router` flags / log lines referring to "kernel" peers.
- README.md / CLAUDE.md prose.
- Any federation topology field that labels a peer endpoint a "kernel".

## Migration discipline

- **Alias-first + backward-compatible fanout.** The router must accept and emit both `kernel` and
  `instance` peer labels across the transition so a mixed fleet (some instances renamed, some not) keeps
  federating. Health/state JSON keys change only behind a compatibility window.
- **Build gate = apply gate (T218).** No rename merges to `master` until `Doctor` + `go test ./...` (plus
  a live fan-out/fan-in smoke against at least one peer) pass on this branch.
- **Boundary.** This branch carries data (this note). Its existence emits no HG rewrite, no deploy, no
  rename, and changes no DNS/Cloudflare/tunnel surface. It is S0 substrate until reviewed and merged with
  a provenance trailer:
  `authored-by: agent:vscode.hp-z440.primary / session:sam.z440-vscode-projection-lead / manifold-instances-vocab`.
