# freedom — HPLC data platform

In-cluster pipeline that turns instrument `.lcd` files (landed in the versitygw
`hplc-raw` bucket by the work PCs, see `../truenas/`) into queryable metrics,
alerts, and dashboards.

```
hplc-raw/<pc>/*.lcd                          (raw, on the TrueNAS box)
     │  hplc-parser (1 pod, 5-min scan, idempotent via files ledger)
     ├─ scalars ─────────────► Postgres  (freedom/hplc-postgres, tables files+runs)
     └─ decoded traces (.npz) ► hplc-derived bucket  (referenced by runs.trace_key)
     │
  sql-exporter (domain gauges) ┐
  postgres-exporter (DB health)┘► kube-prometheus-stack ─► PrometheusRule ─► email
     │
  Grafana ("HPLC Instruments" dash, HPLC folder) ◄─ HPLC Postgres datasource (grafana_ro)
```

Deployment: MANUAL `kubectl apply` (not Flux). Order: `00-namespace`, secrets
(below), `postgres` → load `schema.sql` → `parser` → `exporters` (+ config
secret) → `monitoring` → `dashboard`; datasource secret goes in **monitor** ns.

## Failure detection (empirical, from 3,602 backfilled runs)

**stroke_amp** = FFT amplitude of the pump-stroke band (0.2–0.3 Hz) of the
high-passed 2–14 min pressure trace. It is the clean signal because it is
**method-independent** (pump mechanics, not backpressure).

- Healthy population: stroke_amp ≤ 1.0.  Failures: 3.3–5.5.  **Empty gap 1.25–3.25.**
- Flat threshold **2.0** sits in that gap → zero false positives on 3,602 runs.
- Validated against the known events: the tail is exactly the 8/12 22:39 Helsa
  failure (recurring 8/9→8/13) plus the 8/9 debris precursor.

Alerts (`monitoring.yaml`, severity:critical → existing email-operator receiver):
- **HplcPumpHeadFailure** (critical): `stroke_amp > 2 AND p_2min < 0.8×baseline`
  — pulsation spike + pressure collapse = dead head.
- **HplcPumpStrokeAnomaly** (warning): `stroke_amp > 2 AND pressure normal`
  — developing fault/debris; this is the *precursor* signature.
- **HplcParseErrors / HplcBacklogStuck / HplcExporterDown** (warning): pipeline health.

Keyed on **`pc`** (folder = transport identity), NOT `instrument`: the embedded
instrument string bifurcates (`HPLC` → `DESKTOP-5HLOM1R-Instrument1` around
2026-07-13) so grouping by it splits one physical machine.

## Secrets (imperative — not in repo)

- `hplc-postgres-secret` (username/password) — Bitwarden "freedom postgres (hplc)"
- `versitygw-parser-secret` (access_key/secret_key/endpoint) — Bitwarden "versitygw parser token (freedom)"
- `sql-exporter-config` — rendered from `sql-exporter-config.yaml` with the PG
  password substituted for `__PGPASS__` (the config carries the DSN):
  ```
  PGPASS=$(kubectl get secret hplc-postgres-secret -n freedom -o jsonpath='{.data.password}' | base64 -d)
  sed "s|__PGPASS__|$PGPASS|" sql-exporter-config.yaml > /tmp/c.yaml
  kubectl create secret generic sql-exporter-config -n freedom --from-file=config.yaml=/tmp/c.yaml --dry-run=client -o yaml | kubectl apply -f -
  ```
- `hplc-postgres-grafana-datasource` (monitor ns, label grafana_datasource=1) —
  grafana_ro password, Bitwarden "freedom postgres grafana_ro"

## Parser (Go)

`parser-go/` — the production parser: a static Go binary (distroless image),
built on the arc-chem runner and pushed to `zot.hwcopeland.net/freedom/hplc-parser`
by `.github/workflows/build-hplc-parser.yml`, deployed via `parser-deployment.yaml`.

It is a faithful port of the hplc-tools Python decoder — validated byte-for-byte
against `lcd_parse.py`/`lcd_meta.py`/`pressure_aug.py` on real files across the
healthy→failure range (e.g. TB-500 → stroke_amp 0.436, the 8/13 BAC failure →
5.177, identical to Python). Files: `decode.go` (OLE + chunk-aware delta decode),
`meta.go` (acq_time/instrument), `metrics.go` (metrics + FFT stroke_amp),
`npz.go` (numpy .npz trace writer), `main.go` (S3 + Postgres scan loop).

Local check without deploying: `cd parser-go && go run . validate /path/to/x.lcd`.
The earlier PoC (pip-on-start Python in a ConfigMap) is retired.

## Known follow-ups

- stroke_amp threshold is validated on Helsa only; re-check per instrument as
  more PCs come online (baselines are per-`pc`).
- Convert imperative secrets to ExternalSecrets (like monitor/spotify-postgres).
- Bake the parser image instead of pip-on-start.
- Dashboard x-axis uses acq_at (instrument clock, ~1h offset) — shape correct.
