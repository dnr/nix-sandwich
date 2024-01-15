//go:build !cgo

package main

import "github.com/nix-community/go-nix/pkg/narinfo"

type (
	sysType          int32
	sysChecker       struct{}
	sysCheckerResult struct {
		sys     sysType
		narSize int64
		signer  string
	}
)

const sysUnknown sysType = 0

func newSysChecker(cfg *config) *sysChecker {
	panic("syschecker disabled without cgo")
}
func (s *sysChecker) getSysFromStorePathBatch(storePaths []string) (outs []sysCheckerResult) {
	panic("syschecker disabled without cgo")
}
func (s *sysChecker) getSysFromNarInfo(ni *narinfo.NarInfo) sysType {
	panic("syschecker disabled without cgo")
}
