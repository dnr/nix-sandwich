package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/acomagu/bufpipe"
	"github.com/nix-community/go-nix/pkg/nar"
	"golang.org/x/sync/semaphore"
)

type (
	narExpanderOptions struct {
		BufferEntries int
		BufferBytes   int64
	}

	narExpander struct {
		opts narExpanderOptions
		ents chan *narEntry
		sem  *semaphore.Weighted
	}

	narEntry struct {
		err     error
		h       nar.Header
		r       io.Reader
		release func() error
	}

	narExpanderMeta struct {
		Algo           string   `json:"a"`
		Options        []string `json:"o,omitempty"`
		CompressedSize int64    `json:"c"`
	}

	xzInfo struct {
		uncompressedSize int64
		options          []string
	}
)

const (
	// needs to be lexicographically ordered so use minimal suffix
	narExpMetaSuffix = "\x01_exp1meta_"
	narExpDataSuffix = "\x01_exp2data_"
)

var (
	errBadXzData = errors.New("bad xz data")
	errBadGzData = errors.New("bad gz data")
)

func ExpandNar(r io.Reader, opts narExpanderOptions) io.Reader {
	opts.defaults()
	pr, pw := io.Pipe()
	n := &narExpander{
		opts: opts,
		ents: make(chan *narEntry, opts.BufferEntries),
		sem:  semaphore.NewWeighted(opts.BufferBytes),
	}
	go n.readAndExpand(r)
	go n.writeEnts(pw)
	return pr
}

func CollapseNar(r io.Reader, opts narExpanderOptions) io.Reader {
	opts.defaults()
	pr, pw := io.Pipe()
	n := &narExpander{
		opts: opts,
		ents: make(chan *narEntry, opts.BufferEntries),
		sem:  semaphore.NewWeighted(opts.BufferBytes),
	}
	go n.readAndCollapse(r)
	go n.writeEnts(pw)
	return pr
}

func (o *narExpanderOptions) defaults() {
	if o.BufferEntries == 0 {
		o.BufferEntries = 4 * runtime.NumCPU()
	}
	if o.BufferBytes == 0 {
		o.BufferBytes = 128 * 1024 * 1024
	}
}

func (n *narExpander) readAndExpand(r io.Reader) (retErr error) {
	defer func() {
		if retErr != nil {
			n.ents <- &narEntry{retErr, nar.Header{}, nil, nil}
		}
		close(n.ents)
	}()

	nr, err := nar.NewReader(r)
	if err != nil {
		return err
	}
	defer nr.Close()

	for {
		h, err := nr.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		switch {
		case h.Type == nar.TypeDirectory || h.Type == nar.TypeSymlink:
			n.ents <- &narEntry{nil, *h, nil, nil}
		case strings.HasSuffix(h.Path, ".xz"):
			if err := n.expandXz(nr, h); err != nil {
				return err
			}
		case strings.HasSuffix(h.Path, ".gz"):
			if err := n.expandGz(nr, h); err != nil {
				return err
			}
		default:
			if err := n.passThrough(nr, h); err != nil {
				return err
			}
		}
	}
}

func (n *narExpander) expandXz(nr *nar.Reader, h *nar.Header) error {
	semSize := min(n.opts.BufferBytes, h.Size)
	n.sem.Acquire(context.Background(), semSize)

	buf, err := readFullFromNar(nr, h)
	if err != nil {
		return err
	}

	xzInfo, err := parseXz(buf)
	if err != nil {
		// pass through instead
		release := func() error { n.sem.Release(semSize); return nil }
		n.ents <- &narEntry{nil, *h, bytes.NewReader(buf), release}
		return nil
	}

	meta := narExpanderMeta{
		Algo:           "xz",
		Options:        xzInfo.options,
		CompressedSize: h.Size,
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	metaHeader := *h
	metaHeader.Path += narExpMetaSuffix
	metaHeader.Size = int64(len(metaData))
	n.ents <- &narEntry{nil, metaHeader, bytes.NewReader(metaData), nil}

	dataHeader := *h
	dataHeader.Path += narExpDataSuffix
	dataHeader.Size = xzInfo.uncompressedSize

	xz := exec.Command(xzBin, "-dc")
	xz.Stderr = os.Stderr
	xz.Stdin = bytes.NewReader(buf)
	uncompressedReader, err := xz.StdoutPipe()
	if err != nil {
		return err
	}
	if err := xz.Start(); err != nil {
		return err
	}
	release := func() error {
		defer n.sem.Release(semSize)
		return xz.Wait()
	}
	n.ents <- &narEntry{nil, dataHeader, uncompressedReader, release}

	return nil
}

func (n *narExpander) expandGz(nr *nar.Reader, h *nar.Header) error {
	// TODO: factor out common parts between this and expandXz
	semSize := min(n.opts.BufferBytes, h.Size)
	n.sem.Acquire(context.Background(), semSize)

	buf, err := readFullFromNar(nr, h)
	if err != nil {
		return err
	}

	// gzip, deflate, no flags, 0 mtime, unix
	if len(buf) < 18 || !bytes.Equal(buf[:10], []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3}) {
		// pass through instead
		release := func() error { n.sem.Release(semSize); return nil }
		n.ents <- &narEntry{nil, *h, bytes.NewReader(buf), release}
		return nil
	}

	end := len(buf)
	uncmpSize := binary.LittleEndian.Uint32(buf[end-4:])

	meta := narExpanderMeta{
		Algo:           "gz",
		CompressedSize: h.Size,
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	metaHeader := *h
	metaHeader.Path += narExpMetaSuffix
	metaHeader.Size = int64(len(metaData))
	n.ents <- &narEntry{nil, metaHeader, bytes.NewReader(metaData), nil}

	dataHeader := *h
	dataHeader.Path += narExpDataSuffix
	dataHeader.Size = int64(uncmpSize)

	gz := exec.Command(gzipBin, "-ndc")
	gz.Stderr = os.Stderr
	gz.Stdin = bytes.NewReader(buf)
	uncompressedReader, err := gz.StdoutPipe()
	if err != nil {
		return err
	}
	if err := gz.Start(); err != nil {
		return err
	}
	release := func() error {
		defer n.sem.Release(semSize)
		return gz.Wait()
	}
	n.ents <- &narEntry{nil, dataHeader, uncompressedReader, release}

	return nil
}

func (n *narExpander) readAndCollapse(r io.Reader) (retErr error) {
	defer func() {
		if retErr != nil {
			n.ents <- &narEntry{retErr, nar.Header{}, nil, nil}
		}
		close(n.ents)
	}()

	nr, err := nar.NewReader(r)
	if err != nil {
		return err
	}
	defer nr.Close()

	for {
		h, err := nr.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		switch {
		case h.Type == nar.TypeDirectory || h.Type == nar.TypeSymlink:
			n.ents <- &narEntry{nil, *h, nil, nil}

		case strings.HasSuffix(h.Path, narExpMetaSuffix):
			meta, err := n.readMeta(nr, h)
			if err != nil {
				return err
			}
			h, err = nr.Next()
			if err == io.EOF {
				return io.ErrUnexpectedEOF
			} else if err != nil {
				return err
			} else if !strings.HasSuffix(h.Path, narExpDataSuffix) {
				return errors.New("bad expanded nar")
			}
			switch meta.Algo {
			case "xz":
				err = n.recompressXz(nr, h, meta)
				if err != nil {
					return err
				}
			case "gz":
				err = n.recompressGz(nr, h, meta)
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("unexpected algo %q", meta.Algo)
			}

		default:
			if err := n.passThrough(nr, h); err != nil {
				return err
			}
		}
	}
}

func (n *narExpander) readMeta(nr *nar.Reader, h *nar.Header) (*narExpanderMeta, error) {
	var meta narExpanderMeta
	err := json.NewDecoder(nr).Decode(&meta)
	return &meta, err
}

func (n *narExpander) recompressXz(nr *nar.Reader, h *nar.Header, meta *narExpanderMeta) error {
	semSize := min(n.opts.BufferBytes, h.Size+meta.CompressedSize)
	n.sem.Acquire(context.Background(), semSize)

	buf, err := readFullFromNar(nr, h)
	if err != nil {
		return err
	}

	newH := *h
	newH.Path = strings.TrimSuffix(h.Path, narExpDataSuffix)
	newH.Size = meta.CompressedSize

	xz := exec.Command(xzBin, append([]string{"-c"}, meta.Options...)...)
	xz.Stderr = os.Stderr
	xz.Stdin = bytes.NewReader(buf)
	// note that the buffer in bufpipe will grow without bound, but we know it'll be smaller
	// than buf so it's okay.
	pr, pw := bufpipe.New(make([]byte, 0, 4096))
	xz.Stdout = pw
	if err := xz.Start(); err != nil {
		return err
	}
	go func() { pw.CloseWithError(xz.Wait()) }()
	release := func() error { n.sem.Release(semSize); return nil }
	n.ents <- &narEntry{nil, newH, pr, release}

	return nil
}

func (n *narExpander) recompressGz(nr *nar.Reader, h *nar.Header, meta *narExpanderMeta) error {
	// TODO: factor out common parts between this and expandXz
	semSize := min(n.opts.BufferBytes, h.Size+meta.CompressedSize)
	n.sem.Acquire(context.Background(), semSize)

	buf, err := readFullFromNar(nr, h)
	if err != nil {
		return err
	}

	newH := *h
	newH.Path = strings.TrimSuffix(h.Path, narExpDataSuffix)
	newH.Size = meta.CompressedSize

	gz := exec.Command(gzipBin, "-nc")
	gz.Stderr = os.Stderr
	gz.Stdin = bytes.NewReader(buf)
	// note that the buffer in bufpipe will grow without bound, but we know it'll be smaller
	// than buf so it's okay.
	pr, pw := bufpipe.New(make([]byte, 0, 4096))
	gz.Stdout = pw
	if err := gz.Start(); err != nil {
		return err
	}
	go func() { pw.CloseWithError(gz.Wait()) }()
	release := func() error { n.sem.Release(semSize); return nil }
	n.ents <- &narEntry{nil, newH, pr, release}

	return nil
}

func (n *narExpander) passThrough(nr *nar.Reader, h *nar.Header) error {
	semSize := min(n.opts.BufferBytes, h.Size)
	n.sem.Acquire(context.Background(), semSize)
	release := func() error { n.sem.Release(semSize); return nil }

	buf, err := readFullFromNar(nr, h)
	if err != nil {
		return err
	}
	n.ents <- &narEntry{nil, *h, bytes.NewReader(buf), release}
	return nil
}

func (n *narExpander) writeEnts(w *io.PipeWriter) (retErr error) {
	defer func() {
		w.CloseWithError(retErr)
	}()
	nw, err := nar.NewWriter(w)
	if err != nil {
		return err
	}
	buf := make([]byte, 128*1024)
	for ent := range n.ents {
		if ent.err != nil {
			return ent.err
		}
		if err := nw.WriteHeader(&ent.h); err != nil {
			return err
		}
		if ent.r != nil {
			if err := ioCopy(nw, ent.r, buf, ent.h.Size); err != nil {
				return fmt.Errorf("ExpandNar: %s: %w", ent.h.Path, err)
			}
		}
		if ent.release != nil {
			if err := ent.release(); err != nil {
				return err
			}
		}
	}
	return nw.Close()
}

func parseXz(buf []byte) (xzInfo, error) {
	// https://tukaani.org/xz/xz-file-format.txt
	// https://stackoverflow.com/questions/27000695/is-xz-file-format-description-telling-it-all
	if len(buf) < 32 || !bytes.Equal(buf[:6], []byte{0xFD, '7', 'z', 'X', 'Z', 0x00}) {
		return xzInfo{}, fmt.Errorf("%w: bad magic", errBadXzData)
	}

	var opts []string

	checkType := buf[7] & 0xf
	switch checkType {
	case 0x00:
		opts = append(opts, "--check=none")
	case 0x01:
		opts = append(opts, "--check=crc32")
	case 0x04:
		opts = append(opts, "--check=crc64")
	case 0x0A:
		opts = append(opts, "--check=sha256")
	default:
		return xzInfo{}, fmt.Errorf("%w: unknown checkType %v", errBadXzData, checkType)
	}
	// checkLen := 1 << ((checkType + 5) / 3)
	// if checkType == 0 {
	// 	checkLen = 0
	// }

	// block starts at buf[12]
	// bHdrSize := (int(buf[12]) + 1) * 4
	bFlags := buf[13]
	nFilters := (bFlags & 0x03) + 1
	hasCmpSize := bFlags&0x40 != 0
	hasUncmpSize := bFlags&0x80 != 0

	i := 14
	if hasCmpSize {
		_, l := readVarint(buf[i:]) // compressed size
		i += l
	}
	if hasUncmpSize {
		_, l := readVarint(buf[i:]) // uncompressed size
		i += l
	}
	// get filter flags from first block
	for filt := 0; filt < int(nFilters); filt++ {
		filterId, l := readVarint(buf[i:])
		i += l
		propSize, l := readVarint(buf[i:])
		i += l

		switch filterId {
		case 0x21: // lzma2
			if propSize != 1 {
				return xzInfo{}, fmt.Errorf("%w: lzma2 filter has wrong propSize %v", errBadXzData, propSize)
			}
			dictSize := int(1<<32 - 1)
			bits := int(buf[i] & 0x3F)
			if bits > 40 {
				return xzInfo{}, fmt.Errorf("%w: lzma2 filter has bad dictSize %v", errBadXzData, bits)
			} else if bits < 40 {
				dictSize = (2 | (bits & 1)) << (bits/2 + 11)
			}
			opts = append(opts, fmt.Sprintf("--lzma2=dict=%d", dictSize))

		case 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a: // bcj
			// TODO: support start= option for bcj
			tab := map[uint64]string{
				0x04: "--x86", 0x05: "--powerpc", 0x06: "--ia64", 0x07: "--arm",
				0x08: "--armthumb", 0x09: "--sparc", 0x0a: "--arm64",
			}
			opts = append(opts, tab[filterId])

		case 0x03: // delta
			if propSize != 1 {
				return xzInfo{}, fmt.Errorf("%w: delta filter has wrong propSize %v", errBadXzData, propSize)
			}
			opts = append(opts, fmt.Sprintf("--delta=dist=%d", buf[i]+1))

		default:
			// this should only happen for an empty file?
			// return xzInfo{}, fmt.Errorf("%w: unknown filter %v", errBadXzData, filterId)
		}

		i += int(propSize)
	}

	// go to footer
	end := len(buf)
	if !bytes.Equal(buf[end-2:], []byte{'Y', 'Z'}) ||
		!bytes.Equal(buf[end-4:end-2], buf[6:8]) {
		return xzInfo{}, fmt.Errorf("%w: bad footer magic or mismatch stream flags", errBadXzData)
	}
	bwSize := int((binary.LittleEndian.Uint32(buf[end-8:end-4]) + 1) * 4)
	if end-12-bwSize < 12 {
		return xzInfo{}, fmt.Errorf("%w: too big index size %v", errBadXzData, bwSize)
	}
	index := buf[end-12-bwSize : end-12]
	if index[0] != 0x00 {
		return xzInfo{}, fmt.Errorf("%w: index corrupted %v", errBadXzData, index[0])
	}
	i = 1
	nRec, l := readVarint(index[i:])
	i += l
	var totalUncompressed int64
	for ent := 0; ent < int(nRec); ent++ {
		_, l := readVarint(index[i:]) // unpadded size
		i += l
		uncompressedSize, l := readVarint(index[i:])
		i += l
		totalUncompressed += int64(uncompressedSize)
	}

	return xzInfo{
		uncompressedSize: totalUncompressed,
		options:          opts,
	}, nil
}

func readFullFromNar(nr *nar.Reader, h *nar.Header) ([]byte, error) {
	buf := make([]byte, h.Size)
	num, err := io.ReadFull(nr, buf)
	if err != nil {
		return nil, err
	} else if num != int(h.Size) {
		return nil, io.ErrUnexpectedEOF
	}
	return buf, nil
}

func readVarint(b []byte) (n uint64, l int) {
	for {
		n |= uint64(b[l]&0x7f) << (l * 7)
		if b[l]&0x80 == 0 {
			return n, l + 1
		}
		l++
	}
}
