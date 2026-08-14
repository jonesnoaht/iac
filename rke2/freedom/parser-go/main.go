package main

// hplc-parser (Go) — scan the hplc-raw bucket, decode new .lcd files, write
// per-run scalar metrics to Postgres and full decoded traces to hplc-derived.
// Behaviour matches the Python PoC it replaces; this is the production form.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var errNoPressure = errors.New("no pressure trace")

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

type Config struct {
	Raw, Derived           string
	ScanInterval, MinAge   int
	S3Endpoint             string
	S3Access, S3Secret     string
	S3Secure               bool
}

func loadConfig() Config {
	ep := env("S3_ENDPOINT", "http://versitygw.truenas.svc:7070")
	secure := strings.HasPrefix(ep, "https://")
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	return Config{
		Raw:          env("RAW_BUCKET", "hplc-raw"),
		Derived:      env("DERIVED_BUCKET", "hplc-derived"),
		ScanInterval: envInt("SCAN_INTERVAL", 300),
		MinAge:       envInt("MIN_AGE", 600),
		S3Endpoint:   ep,
		S3Access:     os.Getenv("S3_ACCESS_KEY"),
		S3Secret:     os.Getenv("S3_SECRET_KEY"),
		S3Secure:     secure,
	}
}

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "validate" {
		validateFile(os.Args[2])
		return
	}
	if len(os.Args) >= 3 && os.Args[1] == "pda" {
		pdaFile(os.Args[2])
		return
	}
	cfg := loadConfig()
	ctx := context.Background()

	s3, err := minio.New(cfg.S3Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3Access, cfg.S3Secret, ""),
		Secure: cfg.S3Secure,
	})
	if err != nil {
		log.Fatalf("s3 client: %v", err)
	}

	dsn := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable",
		env("PGUSER", "hplc"), os.Getenv("PGPASSWORD"),
		env("PGHOST", "hplc-postgres"), env("PGDATABASE", "hplc"))
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("pg pool: %v", err)
	}
	defer pool.Close()

	log.Printf("hplc-parser (go) up. raw=%s derived=%s interval=%ds min_age=%ds endpoint=%s",
		cfg.Raw, cfg.Derived, cfg.ScanInterval, cfg.MinAge, cfg.S3Endpoint)

	for {
		if err := scan(ctx, cfg, s3, pool); err != nil {
			log.Printf("scan loop error: %v", err)
		}
		time.Sleep(time.Duration(cfg.ScanInterval) * time.Second)
	}
}

func scan(ctx context.Context, cfg Config, s3 *minio.Client, pool *pgxpool.Pool) error {
	// current ledger
	seen := map[string]string{}
	rows, err := pool.Query(ctx, "SELECT path, status FROM files")
	if err != nil {
		return err
	}
	for rows.Next() {
		var p, st string
		if err := rows.Scan(&p, &st); err == nil {
			seen[p] = st
		}
	}
	rows.Close()

	now := time.Now()
	done, errc, total := 0, 0, 0
	for obj := range s3.ListObjects(ctx, cfg.Raw, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		key := obj.Key
		if !strings.HasSuffix(strings.ToLower(key), ".lcd") {
			continue
		}
		total++
		if seen[key] == "ok" {
			continue
		}
		if now.Sub(obj.LastModified) < time.Duration(cfg.MinAge)*time.Second {
			continue
		}
		// instrument identity = first path segment under hplc-raw/ (the bucket
		// folder we name helsa/hope), NOT the embedded LabSolutions system string.
		inst := key
		if i := strings.IndexByte(key, '/'); i >= 0 {
			inst = key[:i]
		}
		_, _ = pool.Exec(ctx,
			`INSERT INTO files(path,instrument,size,mtime,status) VALUES($1,$2,$3,$4,'pending')
			 ON CONFLICT(path) DO UPDATE SET size=EXCLUDED.size, mtime=EXCLUDED.mtime`,
			key, inst, obj.Size, float64(obj.LastModified.Unix()))

		if err := processOne(ctx, cfg, s3, pool, key, inst); err != nil {
			errc++
			_, _ = pool.Exec(ctx, "UPDATE files SET status='error', error=$1 WHERE path=$2",
				truncate(err.Error(), 300), key)
			log.Printf("ERROR %s: %v", key, err)
		} else {
			done++
		}
	}
	log.Printf("scan: %d objects, %d ok, %d err", total, done, errc)
	return nil
}

func processOne(ctx context.Context, cfg Config, s3 *minio.Client, pool *pgxpool.Pool, key, inst string) error {
	o, err := s3.GetObject(ctx, cfg.Raw, key, minio.GetObjectOptions{})
	if err != nil {
		return err
	}
	defer o.Close()
	blob := readAll(o)
	streams, err := oleStreams(blob)
	if err != nil {
		return err
	}
	m, chans, err := computeMetrics(streams)
	if err != nil {
		return err
	}
	npz, err := npzBytes(chans)
	if err != nil {
		return err
	}
	// Namespace derived objects by artifact type so keys never collide when
	// other derived types (chromatogram/, spectra/, ...) are added later.
	dkey := "pressure/" + key + ".npz"
	if _, err := s3.PutObject(ctx, cfg.Derived, dkey, bytesReader(npz), int64(len(npz)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return err
	}
	// Full 3D PDA matrix → chromatogram/<key>.npz. Non-fatal: a run without
	// (or with unreadable) PDA data still gets its pressure metrics; chrom_key
	// stays NULL and a later re-parse can fill it in.
	var chromKey *string
	if ckey, cerr := putChromatogram(ctx, cfg, s3, key, streams); cerr != nil {
		log.Printf("chromatogram %s: %v", key, cerr)
	} else if ckey != "" {
		chromKey = &ckey
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO runs(path,instrument,system_id,acq_at,run_min,p_start,p_max,p_min,
			p_2min,ripple,max_drop,stroke_amp,flow_med,flow_std,oven_med,trace_key,chrom_key)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT(path) DO UPDATE SET
			instrument=EXCLUDED.instrument, system_id=EXCLUDED.system_id, acq_at=EXCLUDED.acq_at,
			run_min=EXCLUDED.run_min, p_2min=EXCLUDED.p_2min, stroke_amp=EXCLUDED.stroke_amp,
			trace_key=EXCLUDED.trace_key,
			chrom_key=COALESCE(EXCLUDED.chrom_key, runs.chrom_key)`,
		key, inst, m.SystemID, m.AcqAt, m.RunMin, m.PStart, m.PMax, m.PMin,
		m.P2Min, m.Ripple, m.MaxDrop, m.StrokeAmp, m.FlowMed, m.FlowStd, m.OvenMed, dkey, chromKey)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, "UPDATE files SET status='ok', parsed_at=$1, error=NULL WHERE path=$2",
		float64(time.Now().Unix()), key)
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
