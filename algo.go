package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	zstdName   = "zstd"
	xdeltaName = "xdelta"
)

type (
	DiffAlgo interface {
		Name() string
		SetLevel(int)
		Create(context.Context, CreateArgs) (*DiffStats, error)
		Expand(context.Context, ExpandArgs) (*DiffStats, error)
	}

	CreateArgs struct {
		// TODO: consider *os.File or io.Reader instead?
		Base    string
		Request string
		Output  io.Writer
	}

	ExpandArgs struct {
		Base   io.Reader
		Delta  io.Reader
		Output io.Writer
	}

	xd3Algo struct{ level int }
	zstAlgo struct{ level int }
)

func (a *xd3Algo) Name() string       { return xdeltaName }
func (a *xd3Algo) SetLevel(level int) { a.level = level }

func (a *xd3Algo) Create(ctx context.Context, args CreateArgs) (*DiffStats, error) {
	start := time.Now()
	xdelta := exec.CommandContext(
		ctx,
		xdelta3Bin,
		"-v",                        // verbose
		fmt.Sprintf("-%d", a.level), // level
		"-S", "lzma",                // secondary compression
		"-A",            // disable header
		"-D",            // disable compression detection
		"-c",            // stdout
		"-e",            // encode
		"-s", args.Base, // base
		args.Request,
	)
	cw := countWriter{w: args.Output}
	xdelta.Stdout = &cw
	xdeltaErrPipe, err := xdelta.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("xdelta stderr pipe: %w", err)
	}

	if err = xdelta.Start(); err != nil {
		return nil, fmt.Errorf("xdelta start error pipe: %w", err)
	}

	var stderr bytes.Buffer
	_, copyErr := io.Copy(&stderr, xdeltaErrPipe)

	if err = xdelta.Wait(); err != nil {
		return nil, fmt.Errorf("xdelta return: %w [stderr: %q]", err, stderr.String())
	} else if copyErr != nil {
		return nil, fmt.Errorf("xdelta sterr pipe copy: %w", copyErr)
	}

	stats := &DiffStats{
		DiffSize:   cw.c,
		NarSize:    fileSize(args.Request),
		Algo:       a.Name(),
		Level:      a.level,
		CmpTotalMs: time.Now().Sub(start).Milliseconds(),
		CmpUserMs:  xdelta.ProcessState.UserTime().Milliseconds(),
		CmpSysMs:   xdelta.ProcessState.SystemTime().Milliseconds(),
	}
	return stats, nil
}

func (_ *xd3Algo) Expand(ctx context.Context, args ExpandArgs) (*DiffStats, error) {
	start := time.Now()
	xdelta := exec.CommandContext(
		ctx,
		xdelta3Bin,
		"-v",              // verbose
		"-R",              // disable recompression
		"-c",              // stdout
		"-d",              // decode
		"-s", "/dev/fd/3", // base
	)
	xdelta.Stdin = args.Delta // exec automatically creates pipe + copy goroutine
	xdelta.Stdout = args.Output
	xdeltaErrPipe, err := xdelta.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("xdelta stderr pipe: %w", err)
	}

	baseCopyErrCh := make(chan error, 1)
	var closeAfterStart io.Closer
	if f, ok := args.Base.(*os.File); ok {
		// caller is responsible for closing this, whatever it is
		xdelta.ExtraFiles = []*os.File{f}
		baseCopyErrCh <- nil
	} else {
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		closeAfterStart = pr
		xdelta.ExtraFiles = []*os.File{pr}
		go func() { err := ioCopy(pw, args.Base, nil, -1); pw.Close(); baseCopyErrCh <- err }()
	}

	if xdelta.Start(); err != nil {
		return nil, fmt.Errorf("xdelta start error: %w", err)
	}
	if closeAfterStart != nil {
		closeAfterStart.Close()
	}

	var stderr bytes.Buffer
	_, copyErr := io.Copy(&stderr, xdeltaErrPipe)
	baseCopyErr := <-baseCopyErrCh

	if err = xdelta.Wait(); err != nil {
		return nil, fmt.Errorf("xdelta error: %w", err)
	} else if copyErr != nil {
		return nil, fmt.Errorf("xdelta stderr pipe copy: %w", copyErr)
	} else if baseCopyErr != nil {
		return nil, fmt.Errorf("xdelta base pipe copy: %w", baseCopyErr)
	}

	stats := &DiffStats{
		ExpTotalMs: time.Now().Sub(start).Milliseconds(),
		ExpUserMs:  xdelta.ProcessState.UserTime().Milliseconds(),
		ExpSysMs:   xdelta.ProcessState.SystemTime().Milliseconds(),
	}
	return stats, nil
}

func (a *zstAlgo) Name() string       { return zstdName }
func (a *zstAlgo) SetLevel(level int) { a.level = level }

func (a *zstAlgo) Create(ctx context.Context, args CreateArgs) (*DiffStats, error) {
	start := time.Now()
	zstd := exec.CommandContext(
		ctx,
		zstdBin,
		fmt.Sprintf("-%d", a.level), // level
		"--single-thread",           // improve compression (sometimes?)
		"-c",                        // stdout
		"--patch-from", args.Base,   // base
		args.Request,
	)
	cw := countWriter{w: args.Output}
	zstd.Stdout = &cw
	zstdErrPipe, err := zstd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("zstd stderr pipe: %w", err)
	}

	if err = zstd.Start(); err != nil {
		return nil, fmt.Errorf("zstd start error pipe: %w", err)
	}

	var stderr bytes.Buffer
	_, copyErr := io.Copy(&stderr, zstdErrPipe)

	if err = zstd.Wait(); err != nil {
		return nil, fmt.Errorf("zstd return: %w [stderr: %q]", err, stderr.String())
	} else if copyErr != nil {
		return nil, fmt.Errorf("zstd sterr pipe copy: %w", err)
	}

	stats := &DiffStats{
		DiffSize:   cw.c,
		NarSize:    fileSize(args.Request),
		Algo:       a.Name(),
		Level:      a.level,
		CmpTotalMs: time.Now().Sub(start).Milliseconds(),
		CmpUserMs:  zstd.ProcessState.UserTime().Milliseconds(),
		CmpSysMs:   zstd.ProcessState.SystemTime().Milliseconds(),
	}
	return stats, nil
}

func (_ *zstAlgo) Expand(ctx context.Context, args ExpandArgs) (*DiffStats, error) {
	// zstd requires physical file :(
	baseFile, err := os.CreateTemp("", "basenar")
	if err != nil {
		return nil, err
	}
	defer os.Remove(baseFile.Name())

	err = ioCopy(baseFile, args.Base, nil, -1)
	baseFile.Close()
	if err != nil {
		return nil, err
	}

	start := time.Now()
	zstd := exec.CommandContext(
		ctx,
		zstdBin,
		"--long=30", // allow more memory (1GB)
		"-c",        // stdout
		"-d",        // decode
		"--patch-from",
		baseFile.Name(), // base
	)
	zstd.Stdin = args.Delta // exec automatically creates pipe + copy goroutine
	zstd.Stdout = args.Output
	zstdErrPipe, err := zstd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("zstd stderr pipe: %w", err)
	}
	if zstd.Start(); err != nil {
		return nil, fmt.Errorf("zstd start error: %w", err)
	}

	var stderr bytes.Buffer
	_, copyErr := io.Copy(&stderr, zstdErrPipe)

	if err = zstd.Wait(); err != nil {
		return nil, fmt.Errorf("zstd error: %w [stderr: %q]", err, stderr.String())
	} else if copyErr != nil {
		return nil, fmt.Errorf("zstd stderr pipe copy: %w", copyErr)
	}

	stats := &DiffStats{
		ExpTotalMs: time.Now().Sub(start).Milliseconds(),
		ExpUserMs:  zstd.ProcessState.UserTime().Milliseconds(),
		ExpSysMs:   zstd.ProcessState.SystemTime().Milliseconds(),
	}
	return stats, nil
}

func getAlgo(name string) DiffAlgo {
	switch name {
	case xdeltaName:
		return &xd3Algo{level: 6}
	case zstdName:
		return &zstAlgo{level: 9}
	default:
		return nil
	}
}

func pickAlgo(accept []string) DiffAlgo {
	for _, a := range accept {
		name, level, found := strings.Cut(a, "-")
		if algo := getAlgo(name); algo != nil {
			if found {
				if levelInt, err := strconv.Atoi(level); err == nil {
					algo.SetLevel(levelInt)
				}
			}
			return algo
		}
	}
	return nil
}

type countWriter struct {
	w io.Writer
	c int
}

func (c *countWriter) Write(p []byte) (n int, err error) {
	c.c += len(p)
	return c.w.Write(p)
}

func fileSize(fn string) int {
	if fi, err := os.Stat(fn); err == nil {
		return int(fi.Size())
	}
	return 0
}
