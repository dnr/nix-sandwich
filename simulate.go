package main

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nix-community/go-nix/pkg/nixpath"
	"golang.org/x/sync/semaphore"
)

func simulate(cfg *config, baseFile, reqFile string) error {
	reqData, err := os.ReadFile(reqFile)
	if err != nil {
		return err
	}
	baseData, err := os.ReadFile(baseFile)
	if err != nil {
		return err
	}

	reqLines := strings.Split(string(reqData), "\n")
	baseLines := strings.Split(string(baseData), "\n")

	catalog := newCatalog(cfg)
	catalog.set(baseLines)

	subst := newLocalSubstituter(cfg, catalog)

	if cfg.RunDiffer {
		differ := newDifferServer(cfg)
		go differ.serve()
	}

	ctx := context.Background()
	var success, errors atomic.Int32
	sem := semaphore.NewWeighted(40)
	var wg sync.WaitGroup
	skipped := 0
	for _, req := range reqLines {
		req = strings.TrimSpace(req)
		if req == "" {
			skipped++
			continue
		}
		req = strings.TrimPrefix(req, nixpath.StoreDir)
		req = strings.TrimPrefix(req, "/")
		wg.Add(1)
		go func(req string) {
			defer wg.Done()
			sem.Acquire(ctx, 1)
			defer sem.Release(1)
			if stats, err := subst.request(ctx, req); err != nil {
				log.Print(err)
				errors.Add(1)
			} else {
				log.Printf("req %s -> %s", req[33:], stats.String())
				success.Add(1)
			}
		}(req)
	}
	wg.Wait()

	log.Printf("%d paths total, %d diffed, %d err",
		len(reqLines)-skipped, success.Load(), errors.Load())

	return nil
}
