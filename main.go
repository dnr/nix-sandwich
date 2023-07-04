package main

import (
	"flag"
	"io"
	"os"

	"github.com/aws/aws-lambda-go/lambdaurl"
	"golang.org/x/sync/errgroup"
)

func main() {
	cfg := loadConfig()

	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		d := newDifferServer(cfg)
		lambdaurl.Start(d.getHander())
		return
	}

	sim := flag.String("sim", "", "file with paths to load")
	simBase := flag.String("base", "", "file with base paths")
	analyze := flag.Bool("analyze", false, "analyze logs")
	narExpand := flag.Bool("narexpand", false, "run ExpandNar")
	narCollapse := flag.Bool("narcollapse", false, "run CollapseNar")
	dlSpeed := flag.Float64("dlspeed", 40, "assumed download speed in Mbps")
	flag.Parse()

	switch {
	case *sim != "" || *simBase != "":
		panicIfTrue(*sim == "" || *simBase == "", "must specify -base and -sim")
		panicIfErr(simulate(cfg, *simBase, *sim))
		return
	case *analyze:
		opts := analyzeOptions{dlSpeed: *dlSpeed}
		for _, arg := range flag.Args() {
			analyzeLog(arg, opts)
		}
		return
	case *narExpand:
		panicIfErr2(io.Copy(os.Stdout, ExpandNar(os.Stdin, cfgToNarExpanderOptions(cfg))))
		return
	case *narCollapse:
		panicIfErr2(io.Copy(os.Stdout, CollapseNar(os.Stdin, cfgToNarExpanderOptions(cfg))))
		return
	}

	var g errgroup.Group

	if cfg.RunSubstituter {
		catalog := newCatalog(cfg)
		catalog.start()
		subst := newLocalSubstituter(cfg, catalog)
		g.Go(subst.serve)
	}

	if cfg.RunDiffer {
		differ := newDifferServer(cfg)
		g.Go(differ.serve)
	}

	g.Wait()
}

func cfgToNarExpanderOptions(cfg *config) narExpanderOptions {
	return narExpanderOptions{
		BufferEntries: cfg.NarExpBufferEnt,
		BufferBytes:   cfg.NarExpBufferBytes,
	}
}
