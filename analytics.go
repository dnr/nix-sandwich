package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type (
	AnRecord struct {
		T string     `json:"t,omitempty"`
		R *AnRequest `json:"r,omitempty"`
		D *AnDiff    `json:"d,omitempty"`
	}
	AnRequest struct {
		Id            string         `json:"id,omitempty"`
		ReqStorePath  string         `json:"req,omitempty"`       // requested store path (minus /nix/store)
		NarSize       uint64         `json:"nar,omitempty"`       // nar size from upstream
		FileSize      uint64         `json:"file,omitempty"`      // file size from upstream
		BaseStorePath string         `json:"base,omitempty"`      // base that we picked (if we did)
		DifferRequest *differRequest `json:"differReq,omitempty"` // full request to be sent to differ
		Failed        string         `json:"failed,omitempty"`    // error code
	}
	AnDiff struct {
		Id         string `json:"id,omitempty"`
		*DiffStats `json:"stats,omitempty"`
	}

	DiffStats struct {
		BaseSize   int    `json:"base,omitempty"`
		DiffSize   int    `json:"diff,omitempty"`
		NarSize    int    `json:"nar,omitempty"`
		Algo       string `json:"algo,omitempty"`
		Level      int    `json:"lvl,omitempty"`
		CmpTotalMs int64  `json:"cmpMs,omitempty"`
		ExpTotalMs int64  `json:"expMs,omitempty"`
		CmpUserMs  int64  `json:"cmpU,omitempty"`
		CmpSysMs   int64  `json:"cmpS,omitempty"`
		ExpUserMs  int64  `json:"expU,omitempty"`
		ExpSysMs   int64  `json:"expS,omitempty"`
	}

	analyzeOptions struct {
		dlSpeed float64 // megabits/second
	}
)

func (d *DiffStats) String() string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("%s-%d %d/%d -> %d [cmp %dt %du %ds exp %dt %du %ds]",
		d.Algo, d.Level,
		d.BaseSize, d.NarSize, d.DiffSize,
		d.CmpTotalMs, d.CmpUserMs, d.CmpSysMs,
		d.ExpTotalMs, d.ExpUserMs, d.ExpSysMs,
	)
}

func (d *DiffStats) nonnil() *DiffStats {
	if d == nil {
		return &DiffStats{}
	}
	return d
}

func analyzeLog(fn string, opts analyzeOptions) {
	f, err := os.Open(fn)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	reqmap := map[string]*AnRecord{}
	fmap := map[string]int{}
	var diffed []*AnRecord
	minT := time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)
	maxT := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	var tActual int

	d := json.NewDecoder(f)
	for {
		var rec *AnRecord
		if err = d.Decode(&rec); err == io.EOF {
			break
		} else if err != nil {
			panic(err)
		}
		if t, err := time.Parse(time.RFC3339, rec.T); err == nil {
			if t.Before(minT) {
				minT = t
			}
			if t.After(maxT) {
				maxT = t
			}
		}
		if r := rec.R; r != nil {
			fmap[r.Failed]++
			reqmap[rec.R.Id] = rec
			if r.Failed != failedIdentical {
				tActual += int(r.FileSize)
			}
		} else if d := rec.D; d != nil {
			if rec, ok := reqmap[d.Id]; ok {
				tActual -= int(rec.R.FileSize)
				tActual += d.DiffSize
				rec.D = d
				diffed = append(diffed, rec)
			} else {
				fmt.Println("missing R record: ", d.Id)
			}
		}
	}

	var total int
	for _, v := range fmap {
		total += v
	}

	fmt.Printf("======== %s\n", fn)
	fmt.Printf("time range from %s to %s = %.1fs\n",
		minT.Format(time.RFC3339), maxT.Format(time.RFC3339), maxT.Sub(minT).Seconds())

	i := itoaWithSegments
	fmt.Printf("%s total requested  %s diffed  %s eq  %s not found  %s too small  %s too big  %s no base\n",
		i(total),
		i(len(diffed)),
		i(fmap[failedIdentical]),
		i(fmap[failedNotFound]),
		i(fmap[failedTooSmall]),
		i(fmap[failedTooBig]),
		i(fmap[failedNoBase]),
	)

	var tUncmp, tCmp, tDiff int
	var tCmpT, tCmpU, tCmpS int64
	var tExpT, tExpU, tExpS int64
	for _, d := range diffed {
		if d.D.DiffSize == 0 {
			continue
		}
		tUncmp += int(d.R.NarSize)
		tCmp += int(d.R.FileSize)
		tDiff += d.D.DiffSize
		tCmpT += d.D.CmpTotalMs
		tCmpU += d.D.CmpUserMs
		tCmpS += d.D.CmpSysMs
		tExpT += d.D.ExpTotalMs
		tExpU += d.D.ExpUserMs
		tExpS += d.D.ExpSysMs
	}

	fmt.Printf("uncmp nar %s  cmp nar %s  delta size %s  ratio %.2f\n",
		i(tUncmp), i(tCmp), i(tDiff), float64(tCmp)/float64(tDiff))
	fmt.Printf("actual dl %s  actual ratio %.2f",
		i(tActual), float64(tCmp)/float64(tActual))
	fmt.Printf("  cmp nar dl at %gMbps %.1fs\n",
		opts.dlSpeed, float64(tCmp)*8/(opts.dlSpeed*1e6))
	fmt.Printf("compress t:%.1f u:%.1f s:%.1f  expand t:%.1f u:%.1f s:%.1f\n",
		float64(tCmpT)/1000, float64(tCmpU)/1000, float64(tCmpS)/1000,
		float64(tExpT)/1000, float64(tExpU)/1000, float64(tExpS)/1000,
	)
}

func itoaWithSegments(v int) string {
	s := strconv.Itoa(v)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	l := len(s)
	for j, r := range s {
		b.WriteRune(r)
		if ((l-j-1)/3)%2 == 1 {
			b.WriteRune('\u0333')
		}
	}
	return b.String()
}
