//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/golang/groupcache/lru"
	"github.com/google/brotli/go/cbrotli"
	"github.com/nix-community/go-nix/pkg/nar"
	"github.com/nix-community/go-nix/pkg/nar/ls"
	"github.com/nix-community/go-nix/pkg/narinfo"
	"github.com/nix-community/go-nix/pkg/nixpath"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

type (
	// abstract "system" as integer. 0 always means unknown. others values are not
	// defined and only consistent within one process scope.
	sysType int32

	// we need to check the "system" of similar-named store paths so we don't try to e.g.
	// substitute a 32-bit build with a 64-bit build, or aarch64 for amd64.
	sysChecker struct {
		cfg    *config
		reqSem *semaphore.Weighted

		g         *singleflight.Group
		cacheLock sync.Mutex
		cache     *lru.Cache
	}

	// path is relative path from root of nar/store directory.
	presenceFunc func(path string) nar.NodeType

	sysCheckerResult struct {
		sys     sysType
		narSize int64
		signer  string
	}
)

var (
	libcRE = regexp.MustCompile(`^(glibc-[\d.-]+|musl-[\d.]+)$`)
	// grub has both a host and target
	grubRE = regexp.MustCompile(`^grub-[\d.-]+$`)
)

const (
	sysUnknown sysType = 0

	// for use with presenceFunc
	TypeNone  = nar.NodeType("")
	TypeOther = nar.NodeType("other")

	StoreDirLen = len(nixpath.StoreDir)
)

func newSysChecker(cfg *config) *sysChecker {
	return &sysChecker{
		cfg:    cfg,
		reqSem: semaphore.NewWeighted(20),
		g:      new(singleflight.Group),
		cache:  lru.New(10000),
	}
}

func (s *sysChecker) getSysFromStorePathBatch(storePaths []string) (outs []sysCheckerResult) {
	outs = make([]sysCheckerResult, len(storePaths))
	cmd := exec.Command(nixBin, append([]string{"path-info", "--json"}, storePaths...)...)
	cmd.Stderr = os.Stderr
	r, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	type pathInfoItem struct {
		Path       string   `json:"path"`
		References []string `json:"references"`
		NarSize    int64    `json:"narSize"`
		Signatures []string `json:"signatures"`
	}
	var info []*pathInfoItem
	if err := json.NewDecoder(r).Decode(&info); err != nil {
		log.Print("syschecker json decode error: ", err)
		cmd.Wait()
		return
	}
	if err := cmd.Wait(); err != nil {
		return
	}
	refMap := make(map[string]*pathInfoItem)
	for _, i := range info {
		refMap[i.Path] = i
	}
	for i, storePath := range storePaths {
		item := refMap[storePath]
		outs[i].sys = s.getSysFromPathDeps(
			storePath,
			item.References,
			StoreDirLen+1, // has "/nix/store/"
			s.localPresence,
		)
		outs[i].narSize = item.NarSize
		// just take first signature for now
		if len(item.Signatures) > 0 {
			outs[i].signer, _, _ = strings.Cut(item.Signatures[0], ":")
		}
	}
	return
}

func (s *sysChecker) getSysFromNarInfo(ni *narinfo.NarInfo) sysType {
	return s.getSysFromPathDeps(
		ni.StorePath[StoreDirLen+1:],
		ni.References,
		0, // doesn't have "/nix/store"
		s.listingPresence,
	)
}

func (s *sysChecker) getSysFromPathDeps(
	storePath string,
	deps []string,
	prefix int,
	makePresenceFunc func(storeName string) (presenceFunc, any),
) sysType {
	storeName := storePath[prefix:]
	if grubRE.MatchString(storeName[33:]) {
		return s.getCached(storeName[:32], func() sysType {
			return systemFromGrub(makePresenceFunc(storeName))
		})
	}
	for _, dep := range deps {
		dep = dep[prefix:]
		if libcRE.MatchString(dep[33:]) {
			return s.getCached(dep[:32], func() sysType {
				return systemFromLibc(makePresenceFunc(dep))
			})
		}
	}
	return sysUnknown
}

func (s *sysChecker) getCached(storeHash string, f func() sysType) sysType {
	s.cacheLock.Lock()
	v, ok := s.cache.Get(storeHash)
	s.cacheLock.Unlock()
	if ok {
		return v.(sysType)
	}
	v, err, _ := s.g.Do(storeHash, func() (any, error) {
		v := f()
		s.cacheLock.Lock()
		s.cache.Add(storeHash, v)
		s.cacheLock.Unlock()
		return v, nil
	})
	if err != nil {
		return sysUnknown
	}
	return v.(sysType)
}

// arg should be store name (without /nix/store/)
func (s *sysChecker) localPresence(storeName string) (presenceFunc, any) {
	return func(p string) nar.NodeType {
		fi, err := os.Lstat(path.Join(nixpath.StoreDir, storeName, p))
		if err != nil {
			return TypeNone
		}
		switch fi.Mode().Type() {
		case 0:
			return nar.TypeRegular
		case os.ModeDir:
			return nar.TypeDirectory
		case os.ModeSymlink:
			return nar.TypeSymlink
		default:
			return TypeOther
		}
	}, fmt.Sprintf("local %s", storeName)
}

// arg should be store name (without /nix/store/)
func (s *sysChecker) listingPresence(storeName string) (presenceFunc, any) {
	s.reqSem.Acquire(context.Background(), 1)
	defer s.reqSem.Release(1)

	storeHash := storeName[:32]
	res, err := s.makeListRequest(storeHash)
	if err != nil {
		return nil, nil
	}
	defer res.Body.Close()

	r := res.Body
	switch res.Header.Get("content-encoding") {
	case "br":
		r = cbrotli.NewReader(r)
	}

	root, err := ls.ParseLS(r)
	if err != nil {
		return nil, nil
	}
	return func(p string) nar.NodeType {
		node := &root.Root
		for _, part := range strings.Split(p, "/") {
			node = node.Entries[part]
			if node == nil {
				return TypeNone
			}
		}
		return node.Type
	}, fmt.Sprintf("narinfo %s", storeName)
}

func (s *sysChecker) makeListRequest(storeHash string) (*http.Response, error) {
	u := url.URL{
		Scheme: "https",
		Host:   s.cfg.Upstream,
		Path:   "/" + storeHash + ".ls",
	}
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func systemFromLibc(f presenceFunc, thing any) sysType {
	const (
		zeroArch = iota
		x86_64
		i686
		aarch64
	)
	const (
		zeroLibc = iota << 8
		glibc
		musl
	)
	if f == nil {
		return sysUnknown
	}
	switch {
	case f("lib/ld-linux-x86-64.so.2") != TypeNone:
		return glibc | x86_64
	case f("lib/ld-linux-aarch64.so.1") != TypeNone:
		return glibc | aarch64
	case f("lib/ld-linux.so.2") != TypeNone:
		return glibc | i686
	case f("lib/ld-musl-x86_64.so.1") != TypeNone:
		return musl | x86_64
	case f("lib/ld-musl-aarch64.so.1") != TypeNone:
		return musl | aarch64
	case f("lib/ld-musl-i386.so.1") != TypeNone:
		return musl | i686 // this doesn't actually exist but whatever
	}
	log.Printf("couldn't find system from %v", thing)
	return sysUnknown
}

func systemFromGrub(f presenceFunc, thing any) sysType {
	const (
		zeroGrub = iota + 20
		x86_64_grub
		i686_grub
	)
	if f == nil {
		return sysUnknown
	}
	// the host binaries could be glibc or musl but don't worry about that,
	// unlikely they'll be mixed on one system.
	switch {
	case f("lib/grub/x86_64-efi") != TypeNone:
		return x86_64_grub
	case f("lib/grub/i386-pc") != TypeNone:
		return i686_grub
	}
	log.Printf("couldn't find system from %v", thing)
	return sysUnknown
}
