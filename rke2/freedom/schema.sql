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
