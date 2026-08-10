# Working on `theswamp` directly (cluster access)

FMB collaborators (`curtisdearing`, `jonesnoaht`) get **namespace-admin in
`theswamp` and nothing else**. RBAC is in `rbac.yaml` (applied by Flux). Two ways
in — pick one.

## Option A — API key (static ServiceAccount token, no browser) ← simplest

The `swamp-dev` ServiceAccount token is the API key. An admin (you) mints it once
and hands it to the collaborator; it cannot be self-served (that would require
namespace access first — chicken/egg).

**Mint a time-boxed token (preferred — it expires):**
```sh
kubectl -n theswamp create token swamp-dev --duration=2160h   # 90 days
```
**…or read the long-lived (non-expiring) token from the Secret in rbac.yaml:**
```sh
kubectl -n theswamp get secret swamp-dev-token -o jsonpath='{.data.token}' | base64 -d; echo
```

**Build a kubeconfig to hand over:**
```sh
API=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
TOKEN=<paste the token from above>

kubectl config --kubeconfig=swamp.kubeconfig set-cluster swamp \
  --server="$API" --insecure-skip-tls-verify=true
kubectl config --kubeconfig=swamp.kubeconfig set-credentials swamp-dev --token="$TOKEN"
kubectl config --kubeconfig=swamp.kubeconfig set-context swamp \
  --cluster=swamp --user=swamp-dev --namespace=theswamp
kubectl config --kubeconfig=swamp.kubeconfig use-context swamp
```
Then they run `KUBECONFIG=swamp.kubeconfig kubectl get pods` — works **only** in
`theswamp`; any verb in any other namespace is denied.

> For TLS verification instead of `--insecure-skip-tls-verify`, pull the cluster
> CA from the token Secret (`...get secret swamp-dev-token -o
> jsonpath='{.data.ca\.crt}'`), base64-decode it to `ca.crt`, and use
> `--certificate-authority=ca.crt --embed-certs=true`.

## Option B — per-user SSO (OIDC, auditable)

Because `curtisdearing` / `jonesnoaht` are in the Authentik **Florida Man
Bioscience** group, they can authenticate as *themselves* via the cluster's OIDC
flow (the same one the `Kubernetes Users` group uses), landing on the same
`theswamp`-admin scope. Needs the `kubelogin` (`kubectl oidc-login`) plugin and a
kubeconfig pointing at `auth.hwcopeland.net`. Use this if you want per-person
attribution instead of a shared key.

## Browser SSO (Authentik apps in this namespace)

These hosts are guarded by Authentik **full-proxy** (HTTPRoute → `authentik-server`
→ app). Membership in **Florida Man Bioscience** or **Infrastructure** is required
unless noted.

| Host | App slug | Upstream |
|------|----------|----------|
| `https://flmanbiosci.net` | `flmanbiosci` | `u4u-edge` → frontend + `/api/v1` → API |
| `https://app.flmanbiosci.net` | `flmanbiosci-app` | same edge |
| `https://sites.flmanbiosci.net` | `site-tracker` | site-tracker:8040 |

**Public (no login)** on the PeptOdyssey hosts (Authentik `skip_path_regex`):

- `/peptodyssey/privacy` — App Store / TestFlight privacy URL
- `/api/v1/health` — health probes
- `/api/v1/healthkit/*` — device-token HealthKit (iOS does not use browser SSO)

**Direct API (no Authentik browser proxy):**

- `https://api.flmanbiosci.net` → `u4u-engine:8000` (scripts / HealthKit base URL)

**iOS device-code OIDC** (separate from web proxy): Authentik application
`peptodyssey` at `https://auth.hwcopeland.net/application/o/peptodyssey/`
(`client_id=peptodyssey`, public client). See `rke2/authentik/blueprints/providers-peptodyssey.yaml`.

Blueprints live under `rke2/authentik/blueprints/` and must be mirrored into
`rke2/authentik/blueprints-configmap.yaml` (what the pod mounts). After editing
both, commit and let Flux reconcile the `authentik` kustomization (or
`helm upgrade` via `rke2/authentik/update.sh` if you operate that path).

## Scope & caveats
- Both kubectl paths bind to the in-namespace `admin` ClusterRole via **RoleBinding** →
  full control inside `theswamp`, **zero** access to any other namespace / nodes
  / cluster resources.
- **HTTPRoute / Gateway API resources** may still require a cluster-admin apply
  path (Flux); the stock `admin` RoleBinding does not always cover
  `gateway.networking.k8s.io` on this cluster.
- **Reachability:** the kube-apiserver must be reachable from wherever they run
  `kubectl` (home LAN / VPN). RBAC does not grant network reachability.
- **Rotate** the static key by deleting + recreating the `swamp-dev-token`
  Secret (or just use short `create token` durations and skip the Secret).
