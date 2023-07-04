package main

const (
	differPath = "/nix-sandwich-differ"

	differHeaderName  = "header"
	differBodyName    = "body"
	differTrailerName = "trailer"

	narFilterExpandV2 = "expv2"

	// analytics fields
	failedNotFound  = "notfound"  // not found in upstream
	failedTooSmall  = "toosmall"  // too small to bother with
	failedTooBig    = "toobig"    // too big for server to handle
	failedNoBase    = "nobase"    // no local base
	failedIdentical = "identical" // idential (in simulation)
)

var (
	// binary paths (can be overridden by ldflags)
	catBin     = "cat"
	gzipBin    = "gzip"
	nixBin     = "nix"
	xdelta3Bin = "xdelta3"
	xzBin      = "xz"
	zstdBin    = "zstd"
)
