package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/btree"
	"github.com/nix-community/go-nix/pkg/narinfo"
	"github.com/nix-community/go-nix/pkg/nixbase32"
	"github.com/nix-community/go-nix/pkg/nixpath"
)

type (
	btItem struct {
		rest string
		hash [20]byte
		sys  sysType
	}

	catalog struct {
		cfg *config
		bt  atomic.Value // *btree.BTreeG[btItem]

		sysChecker *sysChecker
	}

	reList []*regexp.Regexp
)

var (
	// packages that contain .xz files
	useExpandNarREs = reList{
		// kernel itself (xz)
		regexp.MustCompile(`^linux-[\d.-]+$`),
		// firmware packages (xz)
		regexp.MustCompile(`^alsa-firmware-[\d.-]+-xz$`),
		regexp.MustCompile(`^libreelec-dvb-firmware-[\d.-]+-xz$`),
		regexp.MustCompile(`^rtl8192su-unstable-[\d.-]+-xz$`),
		regexp.MustCompile(`^rt5677-firmware-xz$`),
		regexp.MustCompile(`^intel2200BGFirmware-[\d.-]+-xz$`),
		regexp.MustCompile(`^rtw88-firmware-unstable-[\d.-]+-xz$`),
		regexp.MustCompile(`^linux-firmware-[\d.-]+-xz$`), // this one is huge
		regexp.MustCompile(`^wireless-regdb-[\d.-]+-xz$`),
		regexp.MustCompile(`^sof-firmware-[\d.-]+-xz$`),
		regexp.MustCompile(`^zd1211-firmware-[\d.-]+-xz$`),
		// separate kernel modules (xz)
		regexp.MustCompile(`^v4l2loopback-unstable-[\d.-]+$`),
		// man pages (gz)
		regexp.MustCompile(`^.*-.*-man$`),
	}
	skipREs = reList{
		// compressed single files won't diff well anyway
		regexp.MustCompile(`\.(drv|lock|bz2|gz|xz)$`),
	}
)

func itemLess(a, b btItem) bool {
	return a.rest < b.rest || (a.rest == b.rest && bytes.Compare(a.hash[:], b.hash[:]) < 0)
}

func newCatalog(cfg *config) *catalog {
	c := &catalog{cfg: cfg, sysChecker: newSysChecker(cfg)}
	c.bt.Store(btree.NewG[btItem](4, itemLess))
	return c
}

func (c *catalog) start() {
	c.update()
	go func() {
		for range time.NewTicker(c.cfg.CatalogUpdateFreq).C {
			c.update()
		}
	}()
}

// use only start or set, not both
func (c *catalog) set(names []string) {
	bt := c.bt.Load().(*btree.BTreeG[btItem])
	nt := bt.Clone()
	c.addBatch(nt, names)
	c.bt.Store(nt)
}

func (c *catalog) update() {
	start := time.Now()

	f, err := os.Open(nixpath.StoreDir)
	if err != nil {
		log.Print("catalog list error: ", err)
		return
	}
	defer f.Close()

	bt := c.bt.Load().(*btree.BTreeG[btItem])
	nt := bt.Clone() // safe since this is only called from start goroutine

	for {
		names, err := f.Readdirnames(2048)
		if err != nil && err != io.EOF {
			log.Print("catalog readdirnames: ", err)
			return
		}
		c.addBatch(nt, names)
		if err == io.EOF {
			break
		}
	}
	// TODO: remove names that we didn't find this time

	c.bt.Store(nt)

	log.Printf("catalog updated: %d paths in %.2fs", nt.Len(), time.Since(start).Seconds())
}

func (c *catalog) addBatch(nt *btree.BTreeG[btItem], names []string) {
	var batch []btItem
	var storepaths []string
outer:
	for _, n := range names {
		n = strings.TrimPrefix(n, nixpath.StoreDir)
		n = strings.TrimPrefix(n, "/")

		// TODO: use go-nix/nixpath to parse this?
		if hash, rest, found := strings.Cut(n, "-"); found {
			if useExpandNarREs.matchAny(rest) {
				// allow
			} else if skipREs.matchAny(rest) {
				continue outer
			}
			item := btItem{rest: rest}
			binHash, err := nixbase32.DecodeString(hash)
			if err != nil || len(binHash) != len(item.hash) {
				log.Printf("bad hash %q", hash)
				continue outer
			}
			copy(item.hash[:], binHash)
			if !nt.Has(item) {
				batch = append(batch, item)
				storepaths = append(storepaths, nixpath.StoreDir+"/"+n)
			}
		}
	}
	if len(storepaths) > 0 {
		for i, sys := range c.sysChecker.getSysFromStorePathBatch(storepaths) {
			batch[i].sys = sys
			nt.ReplaceOrInsert(batch[i])
		}
	}
}

func (c *catalog) findBase(ni *narinfo.NarInfo, req string) (string, string, error) {
	if len(req) < 3 {
		return "", "", errors.New("name too short")
	} else if req == "source" {
		// TODO: need contents similarity for this one
		return "", "", errors.New("can't handle 'source'")
	}

	reqSys := c.sysChecker.getSysFromNarInfo(ni)

	// The "name" part of store paths sometimes has a nice pname-version split like
	// "rsync-3.2.6". But also can be something like "rtl8723bs-firmware-2017-04-06-xz" or
	// "sane-desc-generate-entries-unsupported-scanners.patch" or
	// "python3.10-websocket-client-1.4.1" or "lz4-1.9.4-dev" or of course just "source".
	//
	// So given another store path name, how do we find suitable candidates? We're looking for
	// something where just the version has changed, or maybe an exact match of the name. Let's
	// look at segments separated by dashes.  We can definitely reject anything that doesn't
	// share at least one segment. We should also reject anything that doesn't have the same
	// number of segments, since those are probably other outputs or otherwise separate things.
	// Then we can pick one that has the most segments in common.
	//
	// TODO: pick more than one and let differ pick the best based on contents similarity

	dashes := findDashes(req)
	var start string
	if len(dashes) == 0 {
		start = req
	} else {
		start = req[:dashes[0]+1]
	}

	var bestmatch int
	var best btItem

	// look at everything that matches up to the first dash
	bt := c.bt.Load().(*btree.BTreeG[btItem])
	bt.AscendRange(
		btItem{rest: start},
		btItem{rest: start + "\xff"},
		func(i btItem) bool {
			if i.sys == reqSys && len(findDashes(i.rest)) == len(dashes) {
				// take last best instead of first since it's probably more recent
				if match := matchLen(req, i.rest); match >= bestmatch {
					bestmatch = match
					best = i
				}
			}
			return true
		})

	if best.rest == "" {
		return "", "", errors.New("no base found for " + req)
	}

	var narFilter, filterMsg string
	if useExpandNarREs.matchAny(best.rest) {
		narFilter = narFilterExpandV2
		filterMsg = " [expanded]"
	}

	log.Printf("catalog found base for %s -> %s%s", req, best.rest, filterMsg)
	hash := nixbase32.EncodeToString(best.hash[:])
	storePath := nixpath.StoreDir + "/" + hash + "-" + best.rest
	return storePath, narFilter, nil
}

func findDashes(s string) []int {
	var dashes []int
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '-')
		if j < 0 {
			break
		}
		dashes = append(dashes, i+j)
		i += j + 1
	}
	return dashes
}

func matchLen(a, b string) int {
	i := 0
	for ; i < len(a) && i < len(b) && a[i] == b[i]; i++ {
	}
	return i
}

func (l reList) matchAny(s string) bool {
	for _, re := range l {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
