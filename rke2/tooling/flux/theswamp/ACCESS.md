# Working on `theswamp` directly (cluster access)

FMB collaborators (`curtisdearing`, `jonesnoaht`) get **namespace-admin in
`theswamp` and nothing else**. RBAC is in `rbac.yaml` (applied by Flux). Two ways
in — pick one.

## Option A — API key (static ServiceAccount token, no browser) ← simplest

```sh
kubectl -n theswamp create token swamp-dev --duration=2160h   # 90 days
```

Build a kubeconfig (see git history / prior ACCESS for full snippet). Works
**only** in `theswamp`.

## Option B — per-user SSO (OIDC, auditable)

Authentik **Florida Man Bioscience** group + `kubelogin` against
`auth.hwcopeland.net`.

## Host topology (PeptOdyssey subdomain)

| Host | Auth | Backend |
|------|------|---------|
| `https://peptodyssey.flmanbiosci.net` | Authentik full-proxy (FMB \| Infrastructure) | `u4u-edge` → FE + `/api/v1` |
| `https://api-peptodyssey.flmanbiosci.net` | none (device token / scripts) | `u4u-engine:8000` |
| `https://api.flmanbiosci.net` | none (legacy alias) | same API |
| `https://flmanbiosci.net` | **public** company | frontend marketing + product UI 301s |
| `https://flmanbiosci.net/api/v1/*` | none (legacy dual-route) | direct `u4u-engine` (prefix stripped) |
| `https://app.flmanbiosci.net` | 301 → product host | — |
| `https://sites.flmanbiosci.net` | **302** → `sites.floridamanweb.online` | legacy desk |
| `https://sites.floridamanweb.online` | Authentik (site-tracker) | FMWS desk (canonical) |
| `https://ai411.floridamanweb.online` | **public** | AI 411 landing + callback form |
| `https://floridamanweb.online` | **public** | Hashed demo sites + `/ai411/` |
| `https://voice.flmanbiosci.net` | Twilio signature | FMWS voice/SMS/API |
| `https://cytogate.flmanbiosci.net` | **public** landing | portfolio landing |
| `https://u4u-privacy.flmanbiosci.net` | **public** landing | portfolio landing |
| `https://nanodisk.flmanbiosci.net` | **public** landing | MSP / vector nanodisk (research) |
| `https://drug-design.flmanbiosci.net` | **public** lab platform | next-gen drug design |
| `https://drug-design.flmanbiosci.net/protein-chemistry/` | **public** page | VR protein chemistry |

### Why `api-peptodyssey` (hyphen), not `api.peptodyssey`

X.509 / Cloudflare Universal SSL wildcards match **one** label. `*.flmanbiosci.net`
covers `api-peptodyssey.flmanbiosci.net` but **not** `api.peptodyssey.flmanbiosci.net`.

### Public on product host (Authentik `skip_path_regex`)

- `/privacy` and `/peptodyssey/privacy` — App Store / TestFlight
- `/api/v1/health`
- `/api/v1/healthkit` and `/api/v1/healthkit/*` (anchored; backend enforces device token)

### Frozen legacy privacy URL

`https://flmanbiosci.net/peptodyssey/privacy` → **301** →
`https://peptodyssey.flmanbiosci.net/privacy`

### iOS / non-browser API clients

- Privacy: prefer `https://peptodyssey.flmanbiosci.net/privacy` (legacy apex path still redirects)
- **Preferred API base:** `https://api-peptodyssey.flmanbiosci.net` (unprefixed paths: `/health`, `/healthkit/samples`)
- **Legacy dual-routes that still work without cross-host redirects:**
  - `https://api.flmanbiosci.net/...` (unprefixed)
  - `https://flmanbiosci.net/api/v1/...` (prefix stripped at the gateway)
- Do **not** rely on a 301 from apex `/api/v1` to the product host for POST/auth
  clients — URLSession converts POST→GET and strips `Authorization` on cross-host
  redirects. Same-origin `/api/v1` on the product host is fine for browser OIDC.
- Device-code OIDC app remains `peptodyssey` at
  `https://auth.hwcopeland.net/application/o/peptodyssey/`

### Identity headers

Direct API HTTPRoutes and `u4u-edge` strip inbound `X-authentik-*`. The engine
must not trust client-supplied Authentik headers for identity; use OIDC Bearer
or device tokens. Staff browser access is gated by Authentik at the product host.

Blueprints: `rke2/authentik/blueprints/` must stay mirrored in
`blueprints-configmap.yaml` (including CM-only `providers-kubernetes.yaml`).

## FMWS product loop (docs)

Monorepo `demo-websites`:

- `docs/ARCHITECTURE.md` — system design
- `docs/PRODUCT_LOOP.md` — funnel ops
- `docs/OPS_CLUSTER.md` — DNS/Authentik/Flux/secrets runbook
- `docs/API.md` — HTTP APIs

IAC manifests for AI411/sites desk: `httproute-ai411.yaml`,
`httproute-tracker.yaml`, `floridamanweb-dnsrecord.yaml` (ai411 + sites A
records), Authentik `providers-sitetracker.yaml` external_host
`https://sites.floridamanweb.online`.

**Pre-merge:** floridamanweb `sites.` / `ai411.` A records in the manually-
applied kube-system file must be READY before Flux flips the tracker route,
or the legacy host 302s to NXDOMAIN. Ping cluster-admin to apply those DNS
records when ready to merge.

## Scope & caveats

- kubectl RBAC is namespace-admin for core resources; **Gateway HTTPRoutes** often
  still need Flux/cluster-admin.
- kube-apiserver reachability is separate from RBAC (LAN/VPN).
