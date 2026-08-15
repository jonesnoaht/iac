-- HPLC store of record. Scalars only — raw .lcd stays in the hplc-raw bucket,
-- decoded traces/matrices go to the hplc-derived bucket and are referenced here
-- by object key, never inlined.

-- Ingest ledger: one row per .lcd seen, drives idempotent parsing.
CREATE TABLE IF NOT EXISTS files (
  path        TEXT PRIMARY KEY,          -- bucket key, e.g. helsa/2026/08. August/08.10.2026/xxx.lcd
  instrument  TEXT,                      -- instrument name = first path segment (helsa/hope)
  size        BIGINT,
  mtime       DOUBLE PRECISION,          -- object last-modified epoch
  parsed_at   DOUBLE PRECISION,
  status      TEXT NOT NULL DEFAULT 'pending',   -- pending | ok | error
  error       TEXT
);
CREATE INDEX IF NOT EXISTS files_status_idx ON files(status);

-- Per-run scalar metrics. path FK → files. Pressure/QC metrics come from the
-- parser's pressure_metrics(); stroke_amp is the pump-stroke FFT amplitude that
-- spikes on a dead pump head (the alert signal).
CREATE TABLE IF NOT EXISTS runs (
  path         TEXT PRIMARY KEY REFERENCES files(path) ON DELETE CASCADE,
  instrument   TEXT,                     -- instrument name = bucket folder (helsa/hope) — the identity we use everywhere
  system_id    TEXT,                     -- embedded LabSolutions system string (HPLC/DESKTOP-5HLOM1R-*) — demoted, informational only
  acq_at       TIMESTAMPTZ,              -- embedded FILETIME (instrument clock — analysis only, NOT freshness)
  run_min      DOUBLE PRECISION,
  p_start      DOUBLE PRECISION,
  p_max        DOUBLE PRECISION,
  p_min        DOUBLE PRECISION,
  p_2min       DOUBLE PRECISION,
  ripple       DOUBLE PRECISION,
  max_drop     DOUBLE PRECISION,
  stroke_amp   DOUBLE PRECISION,
  flow_med     DOUBLE PRECISION,
  flow_std     DOUBLE PRECISION,
  oven_med     DOUBLE PRECISION,
  -- derived blob references (objects in the hplc-derived bucket)
  trace_key    TEXT,                     -- decoded pressure/flow/oven trace
  chrom_key    TEXT                      -- chromatogram matrix, if extracted
);
CREATE INDEX IF NOT EXISTS runs_instrument_acq_idx ON runs(instrument, acq_at);

-- Read-only role for Grafana.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'grafana_ro') THEN
    CREATE ROLE grafana_ro LOGIN;
  END IF;
END $$;
GRANT CONNECT ON DATABASE hplc TO grafana_ro;
GRANT USAGE ON SCHEMA public TO grafana_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO grafana_ro;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO grafana_ro;

-- Workflow flags derived from the filename (RUSH = priority, (RR) = rerun).
-- GENERATED columns so they apply to every existing row instantly and stay
-- consistent with no parser code. Added live via ALTER (see migration).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS is_rush  BOOLEAN GENERATED ALWAYS AS (path ILIKE '%(RUSH)%') STORED;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS is_rerun BOOLEAN GENERATED ALWAYS AS (path ~* '\(RR\)') STORED;
CREATE INDEX IF NOT EXISTS runs_flags_idx ON runs(is_rush, is_rerun);

-- Real run verdict: pump fault when stroke_amp exceeds the empirical 2.0 line
-- (healthy <=1, failed 3.3-5.5, clean gap between). NULL when stroke_amp is null.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS is_fault BOOLEAN GENERATED ALWAYS AS (stroke_amp > 2) STORED;
CREATE INDEX IF NOT EXISTS runs_fault_idx ON runs(is_fault);

-- Downsampled display traces (~500 pts) for the run-detail dashboard; full-res
-- lives in the pressure/ and chromatogram/ .npz artifacts. Retention time is
-- derived in SQL from run_min and the array index.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS pressure_trace DOUBLE PRECISION[];
ALTER TABLE runs ADD COLUMN IF NOT EXISTS chrom_trace    DOUBLE PRECISION[];
ALTER TABLE runs ADD COLUMN IF NOT EXISTS chrom_nm       DOUBLE PRECISION;

-- Downsampled DAD matrix (~90 wl × 60 rt) for the 3D surface panel; full-res in
-- the chromatogram/.npz. dad_z is flattened rt-major.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS dad_wl DOUBLE PRECISION[];
ALTER TABLE runs ADD COLUMN IF NOT EXISTS dad_rt DOUBLE PRECISION[];
ALTER TABLE runs ADD COLUMN IF NOT EXISTS dad_z  DOUBLE PRECISION[];

-- UV band maxima per run (computed from dad_z) for the impurity scan: peptide
-- band 200-230, aromatic/mid 240-290 (Trp/Tyr or impurity), high 300-450
-- (unambiguously non-peptide chromophore). Backfilled once via UPDATE.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS uv_pep DOUBLE PRECISION;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS uv_mid DOUBLE PRECISION;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS uv_hi  DOUBLE PRECISION;

-- LabSolutions' own peak table, decoded from the .lcd (CR-PDA XML for rt/area/
-- height, PT-PDA binary for reported area-%). These are the numbers on the CoA
-- — the instrument's manual integration, not a re-integration.
CREATE TABLE IF NOT EXISTS peaks (
  path      TEXT NOT NULL REFERENCES runs(path) ON DELETE CASCADE,
  idx       INT  NOT NULL,           -- 0 = main (largest area)
  rt        DOUBLE PRECISION,        -- minutes
  area      DOUBLE PRECISION,
  height    DOUBLE PRECISION,
  area_pct  DOUBLE PRECISION,        -- reported area-% (main peak's = purity)
  PRIMARY KEY (path, idx)
);
CREATE INDEX IF NOT EXISTS peaks_path ON peaks(path);

-- Run-level quant summary + raw-acquisition fingerprint. raw_sha hashes only the
-- acquisition streams (never the peak table), so a re-integration re-upload keeps
-- it stable while a different run on the same key changes it — the safety guard.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS purity     DOUBLE PRECISION;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS n_peaks    INT;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS main_rt    DOUBLE PRECISION;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS main_area  DOUBLE PRECISION;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS integrated BOOLEAN;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS raw_sha    TEXT;
-- designated-target count (from CR non-Original): 1 = single-peptide purity assay
-- (purity is the CoA value); >1 = blend/screen (purity is the main component).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS n_identified INT;
CREATE INDEX IF NOT EXISTS runs_purity_idx ON runs(instrument, integrated, purity);

-- Parser bookkeeping: peaks_done gates the peak-table backfill of already-parsed
-- runs; reintegrated counts re-parses from changed re-uploads. status can now be
-- 'raw_changed' — acquisition data changed under an existing run, held for review.
ALTER TABLE files ADD COLUMN IF NOT EXISTS peaks_done   BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE files ADD COLUMN IF NOT EXISTS reintegrated INT NOT NULL DEFAULT 0;

GRANT SELECT ON peaks TO grafana_ro;
