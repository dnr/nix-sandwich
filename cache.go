package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

func cacheKey(req *differRequest, algo string) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("up=%s\n", req.Upstream)))
	h.Write([]byte(fmt.Sprintf("req=%s\n", req.ReqNarPath)))
	h.Write([]byte(fmt.Sprintf("base=%s\n", req.BaseStorePath)))
	// ideally we would include the base nar hash and req nar hash, but we don't want to keep
	// all the base hashes in memory. just use the size, that'll avoid most instances of
	// different nars for the same input hash. (the rest will show up as either errors when
	// applying the diff, or in the worst case when nix hashes the result.)
	h.Write([]byte(fmt.Sprintf("sizes=%d,%d\n", req.BaseNarSize, req.ReqNarSize)))
	// note this doesn't include the level:
	h.Write([]byte(fmt.Sprintf("algo=%s\n", algo)))
	if len(req.NarFilter) > 0 {
		h.Write([]byte(fmt.Sprintf("filter=%s\n", req.NarFilter)))
	}
	return "v1-" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))[:36]
}
