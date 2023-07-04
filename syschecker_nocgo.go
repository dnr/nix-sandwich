//go:build !cgo

package main

import "github.com/nix-community/go-nix/pkg/narinfo"

type (
	sysType    int32
	sysChecker struct{}
)

const sysUnknown sysType = 0

func newSysChecker(cfg *config) *sysChecker {
	panic("syschecker disabled without cgo")
}
func (s *sysChecker) getSysFromStorePathBatch(storePaths []string) (outs []sysType) {
	panic("syschecker disabled without cgo")
}
func (s *sysChecker) getSysFromNarInfo(ni *narinfo.NarInfo) sysType {
	panic("syschecker disabled without cgo")
}
