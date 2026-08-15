-- Chromatogram-feature layer + replay-validated detectors (see UPDATES-GOLIVE.md).
-- Apply as the hplc user:  kubectl exec -i -n freedom deploy/hplc-postgres -- psql -U hplc -d hplc < alerting.sql
--
-- D1 flow-deficit (validated on 19,540-run replay 2026-08-14): rolling-median
-- t0 (12 runs, injection-front amp>2000) > 1.10x trailing [-8d,-1d] baseline
-- AND rolling p_2min < 0.95x baseline. Sustain lives in the alert rule (for: 60m).
-- D2 dead-head stays in the existing HplcPumpHeadFailure rule.

-- Full-resolution chromatogram features. History was backfilled from the npz
-- artifacts (src='npz-backfill'); new runs are materialized from the 500-pt
-- chrom_trace by the sql-exporter's hplc_features_materialized query
-- (src='chrom_trace', t0 quantized to ~0.039 min — fine under a 12-run median).
CREATE TABLE IF NOT EXISTS run_features (
  path    TEXT PRIMARY KEY REFERENCES files(path) ON DELETE CASCADE,
  t0_rt   DOUBLE PRECISION,   -- injection-front apex, 0.15-1.2 min window
  t0_amp  DOUBLE PRECISION,   -- apex height above median; >2000 = usable front
  main_rt DOUBLE PRECISION,   -- tallest peak after 3 min
  main_h  DOUBLE PRECISION,
  src     TEXT NOT NULL DEFAULT 'chrom_trace'
);
GRANT SELECT ON run_features TO grafana_ro;

-- Features for runs not yet in run_features, derived from chrom_trace.
CREATE OR REPLACE VIEW run_features_missing AS
SELECT r.path, d.t0_rt, d.t0_amp, d.main_rt, d.main_h
FROM runs r
CROSS JOIN LATERAL (
  WITH t AS (
    SELECT (u.ord - 1) * r.run_min / NULLIF(array_length(r.chrom_trace, 1) - 1, 0) AS tm,
           u.v - (SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY v2)
                  FROM unnest(r.chrom_trace) v2) AS y
    FROM unnest(r.chrom_trace) WITH ORDINALITY AS u(v, ord)
  )
  SELECT (SELECT tm FROM t WHERE tm > 0.15 AND tm < 1.2 ORDER BY y DESC LIMIT 1) AS t0_rt,
         (SELECT y  FROM t WHERE tm > 0.15 AND tm < 1.2 ORDER BY y DESC LIMIT 1) AS t0_amp,
         (SELECT tm FROM t WHERE tm > 3.0 ORDER BY y DESC LIMIT 1) AS main_rt,
         (SELECT y  FROM t WHERE tm > 3.0 ORDER BY y DESC LIMIT 1) AS main_h
) d
WHERE r.chrom_trace IS NOT NULL AND r.run_min > 0
  AND NOT EXISTS (SELECT 1 FROM run_features rf WHERE rf.path = r.path);

-- Current detector state, one row per instrument. Baselines anchor to now();
-- an instrument idle >8 days gets NULL ratios (= no alert), by design.
-- NaN guards matter: a stray NaN poisons percentile_cont.
-- D3 analyte-absence (validated: only 2 streaks in 8.5 months, both real events —
-- hope 12/3, helsa 7/7 pre-column-death): >=4 of last 5 non-blank runs with
-- main_h < 30000 (normal sample peaks are 300k-4M).
DROP VIEW IF EXISTS detector_state;
CREATE VIEW detector_state AS
SELECT i.instrument,
       cur.t0_med, base.t0_base, cur.p2_med, base.p2_base,
       cur.t0_med / NULLIF(base.t0_base, 0) AS t0_ratio,
       cur.p2_med / NULLIF(base.p2_base, 0) AS p2_ratio,
       absent.absent_ct
FROM (SELECT DISTINCT instrument FROM runs WHERE instrument IS NOT NULL) i
CROSS JOIN LATERAL (
  SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY rf.t0_rt)
           FILTER (WHERE rf.t0_amp > 2000 AND rf.t0_rt <> 'NaN'::float8) AS t0_med,
         percentile_cont(0.5) WITHIN GROUP (ORDER BY w.p_2min)
           FILTER (WHERE w.p_2min > 0) AS p2_med
  FROM (SELECT r.path, r.p_2min FROM runs r
        WHERE r.instrument = i.instrument AND r.acq_at IS NOT NULL
        ORDER BY r.acq_at DESC LIMIT 12) w
  LEFT JOIN run_features rf ON rf.path = w.path
) cur
CROSS JOIN LATERAL (
  SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY rf.t0_rt)
           FILTER (WHERE rf.t0_amp > 2000 AND rf.t0_rt <> 'NaN'::float8) AS t0_base,
         percentile_cont(0.5) WITHIN GROUP (ORDER BY r.p_2min)
           FILTER (WHERE r.p_2min > 0) AS p2_base
  FROM runs r
  LEFT JOIN run_features rf ON rf.path = r.path
  WHERE r.instrument = i.instrument
    AND r.acq_at BETWEEN now() - interval '8 days' AND now() - interval '1 day'
) base
CROSS JOIN LATERAL (
  SELECT count(*) FILTER (WHERE rf.main_h < 30000 AND rf.main_h <> 'NaN'::float8) AS absent_ct
  FROM (SELECT r.path FROM runs r
        WHERE r.instrument = i.instrument AND r.acq_at IS NOT NULL AND r.path !~* 'blank'
        ORDER BY r.acq_at DESC LIMIT 5) w
  LEFT JOIN run_features rf ON rf.path = w.path
) absent;
GRANT SELECT ON detector_state TO grafana_ro;

-- Per-run detector verdicts over history (the alert log). Row-wise laterals are
-- index-bounded (runs_instrument_acq_idx); dashboards filter by acq_at.
DROP VIEW IF EXISTS detector_log;
CREATE VIEW detector_log AS
SELECT r.path, r.instrument, r.acq_at, r.stroke_amp, r.p_2min,
       rf0.main_h, rf0.t0_rt,
       cur.t0_med / NULLIF(base.t0_base, 0) AS t0_ratio,
       cur.p2_med / NULLIF(base.p2_base, 0) AS p2_ratio,
       (cur.t0_med > 1.10 * base.t0_base AND cur.p2_med < 0.95 * base.p2_base) AS d1,
       (r.stroke_amp > 2 AND r.p_2min < 0.8 * base.p2_base) AS d2,
       (r.path !~* 'blank' AND rf0.main_h < 30000 AND rf0.main_h <> 'NaN'::float8) AS d3
FROM runs r
LEFT JOIN run_features rf0 ON rf0.path = r.path
CROSS JOIN LATERAL (
  SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY rf.t0_rt)
           FILTER (WHERE rf.t0_amp > 2000 AND rf.t0_rt <> 'NaN'::float8) AS t0_med,
         percentile_cont(0.5) WITHIN GROUP (ORDER BY w.p_2min)
           FILTER (WHERE w.p_2min > 0) AS p2_med
  FROM (SELECT r2.path, r2.p_2min FROM runs r2
        WHERE r2.instrument = r.instrument AND r2.acq_at <= r.acq_at
        ORDER BY r2.acq_at DESC LIMIT 12) w
  LEFT JOIN run_features rf ON rf.path = w.path
) cur
CROSS JOIN LATERAL (
  SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY rf.t0_rt)
           FILTER (WHERE rf.t0_amp > 2000 AND rf.t0_rt <> 'NaN'::float8) AS t0_base,
         percentile_cont(0.5) WITHIN GROUP (ORDER BY r2.p_2min)
           FILTER (WHERE r2.p_2min > 0) AS p2_base
  FROM runs r2
  LEFT JOIN run_features rf ON rf.path = r2.path
  WHERE r2.instrument = r.instrument
    AND r2.acq_at BETWEEN r.acq_at - interval '8 days' AND r.acq_at - interval '1 day'
) base
WHERE r.acq_at IS NOT NULL;
GRANT SELECT ON detector_log TO grafana_ro;
