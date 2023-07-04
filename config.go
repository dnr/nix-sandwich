package main

import (
	"strings"
	"time"

	"github.com/alfred-landrum/fromenv"
)

type (
	config struct {
		Upstream          string        `env:"nix_sandwich_upstream=cache.nixos.org"`
		Differ            string        `env:"nix_sandwich_differ=http://localhost:7420"`
		DifferBind        string        `env:"nix_sandwich_differ_bind=:7420"`
		SubstituterBind   string        `env:"nix_sandwich_substituter_bind=localhost:7419"`
		CatalogUpdateFreq time.Duration `env:"nix_sandwich_catalog_update_freq=1h"`
		DiffAlgo          string        `env:"nix_sandwich_diff_algo=zstd-3,xdelta-1"`
		MinFileSize       int           `env:"nix_sandwich_min_file_size=16384"`
		MaxFileSize       int           `env:"nix_sandwich_max_file_size=1073741824"` // 1 GiB
		RunSubstituter    bool          `env:"nix_sandwich_run_substituter=true"`
		RunDiffer         bool          `env:"nix_sandwich_run_differ=false"`
		AnalyticsFile     string        `env:"nix_sandwich_analytics_file=default"` // empty string to disable
		NarExpBufferEnt   int           `env:"nix_sandwich_nar_expander_buffer_entries"`
		NarExpBufferBytes int64         `env:"nix_sandwich_nar_expander_buffer_bytes"`
	}
)

func loadConfig() *config {
	var c config
	if err := fromenv.Unmarshal(&c, fromenv.SetFunc(setDuration)); err != nil {
		panic(err)
	}
	if strings.IndexByte(c.Differ, '/') < 0 && strings.IndexByte(c.Differ, ':') < 0 {
		c.Differ = c.Differ + ":7420"
	}
	return &c
}

func setDuration(t *time.Duration, s string) error {
	x, err := time.ParseDuration(s)
	*t = x
	return err
}
