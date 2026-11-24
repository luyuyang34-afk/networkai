// Package config loads runtime configuration from environment variables.
//
// Every setting has a development-friendly default except secrets: the
// service refuses to start without JWT_SECRET so a weak key never ships by
// accident.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr string
	// MetricsAddr is where the indexer serves /metrics (the API serves it on
	// HTTPAddr).
	MetricsAddr string

	DatabaseURL string

	ESURL      string
	ESUsername string
	ESPassword string

	JWTSecret []byte
	JWTTTL    time.Duration

	// MediaBackend is "local" (files on disk, served by the API) or "gcs".
	MediaBackend   string
	MediaDir       string
	MediaBaseURL   string
	GCSBucket      string
	MaxUploadBytes int64

	CORSOrigins []string

	IndexerBatchSize   int
	IndexerPoll        time.Duration
	IndexerMaxAttempts int
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:           env("HTTP_ADDR", ":8080"),
		MetricsAddr:        env("METRICS_ADDR", ":9091"),
		DatabaseURL:        env("DATABASE_URL", "postgres://networkai:networkai@localhost:5432/networkai?sslmode=disable"),
		ESURL:              env("ES_URL", "http://localhost:9200"),
		ESUsername:         os.Getenv("ES_USERNAME"),
		ESPassword:         os.Getenv("ES_PASSWORD"),
		JWTSecret:          []byte(os.Getenv("JWT_SECRET")),
		MediaBackend:       env("MEDIA_BACKEND", "local"),
		MediaDir:           env("MEDIA_DIR", "./data/media"),
		MediaBaseURL:       env("MEDIA_BASE_URL", "http://localhost:8080/media"),
		GCSBucket:          os.Getenv("GCS_BUCKET"),
		CORSOrigins:        splitList(env("CORS_ORIGINS", "http://localhost:3000")),
		IndexerBatchSize:   100,
		IndexerMaxAttempts: 10,
	}

	var errs []error
	c.JWTTTL = durationEnv("JWT_TTL", 24*time.Hour, &errs)
	c.IndexerPoll = durationEnv("INDEXER_POLL_INTERVAL", 500*time.Millisecond, &errs)
	c.MaxUploadBytes = int64(intEnv("MAX_UPLOAD_MB", 50, &errs)) << 20
	c.IndexerBatchSize = intEnv("INDEXER_BATCH_SIZE", c.IndexerBatchSize, &errs)
	c.IndexerMaxAttempts = intEnv("INDEXER_MAX_ATTEMPTS", c.IndexerMaxAttempts, &errs)

	if len(c.JWTSecret) < 32 {
		errs = append(errs, errors.New("JWT_SECRET must be set and at least 32 bytes"))
	}
	switch c.MediaBackend {
	case "local":
	case "gcs":
		if c.GCSBucket == "" {
			errs = append(errs, errors.New("GCS_BUCKET is required when MEDIA_BACKEND=gcs"))
		}
	default:
		errs = append(errs, fmt.Errorf("MEDIA_BACKEND must be local or gcs, got %q", c.MediaBackend))
	}
	return c, errors.Join(errs...)
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int, errs *[]error) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		*errs = append(*errs, fmt.Errorf("%s must be a positive integer, got %q", key, v))
		return def
	}
	return n
}

func durationEnv(key string, def time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s must be a positive duration like 500ms, got %q", key, v))
		return def
	}
	return d
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
