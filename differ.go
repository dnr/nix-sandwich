package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/nix-community/go-nix/pkg/narinfo"
)

type (
	differRequest struct {
		// required for request:
		ReqNarPath    string   `json:"reqNarPath"`            // full nar path of requested
		BaseStorePath string   `json:"baseStorePath"`         // full store path of base
		AcceptAlgos   []string `json:"acceptAlgos,omitempty"` // accepted diff algos
		NarFilter     string   `json:"narFilter,omitempty"`   // pipe nars through a filter
		Upstream      string   `json:"upstream,omitempty"`

		// informational only:
		ReqNarSize int64  `json:"reqNarSize"` // size of requested nar (used for resource control)
		ReqName    string `json:"reqName"`    // requested (name only, no hash) (used for log)
	}

	differServer struct {
		cfg *config
		// dlSem    *semaphore.Weighted
		// deltaSem *semaphore.Weighted
	}

	differHeader struct {
		Algo string
	}

	differTrailer struct {
		Ok    bool
		Stats *DiffStats
		Error string
	}

	readerFilter func(io.Reader) io.Reader
)

var errNotFound = errors.New("not found")

func newDifferServer(cfg *config) *differServer {
	// // roughly, each download will use some network plus an xz process,
	// // and each delta will use an xdelta3/zstd process.
	// // so effectively this will allow about 2×cpus processes to run.
	// concurrency := int64(runtime.NumCPU())
	return &differServer{
		cfg: cfg,
		// dlSem:    semaphore.NewWeighted(concurrency),
		// deltaSem: semaphore.NewWeighted(concurrency),
	}
}

func (d *differServer) getHander() http.Handler {
	h := http.NewServeMux()
	h.HandleFunc(differPath, fw(d.differ))
	return h
}

func (d *differServer) serve() error {
	srv := &http.Server{
		Addr:    d.cfg.DifferBind,
		Handler: d.getHander(),
	}
	return srv.ListenAndServe()
}

func (d *differServer) differ(w http.ResponseWriter, r *http.Request) (retStatus int, retMsg string, retErr error) {
	if r.Method != "POST" {
		return http.StatusMethodNotAllowed, "", nil
	}

	var req differRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return http.StatusBadRequest, "json decode error", err
	}
	if req.Upstream == "" {
		req.Upstream = d.cfg.Upstream
	}
	// TODO: should we do this?
	// if req.Upstream == "cache.nixos.org" && os.Getenv("AWS_REGION") == "us-east-1" {
	// 	// If we're in us-east-1, prefer S3 directly since it's free.
	// 	req.Upstream = "nix-cache.s3.amazonaws.com"
	// }

	// TODO: pick algo based on size or other properties?
	algo := pickAlgo(req.AcceptAlgos)
	if algo == nil {
		return http.StatusBadRequest, "unknown algo", nil
	}

	// download base + req nar
	expFilter, _ := getNarFilter(d.cfg, &req)

	reqNar, reqSize, reqClose, err := d.streamNar(
		req.Upstream, req.ReqName, req.ReqNarPath, req.ReqNarSize, expFilter)
	if err != nil {
		return http.StatusInternalServerError, "nar start download error", err
	}
	defer reqClose()

	hash, _, _ := strings.Cut(path.Base(req.BaseStorePath), "-")
	baseNar, baseSize, baseClose, err := d.streamNarFromInfo(req.Upstream, hash, expFilter)
	if err != nil {
		if err == errNotFound {
			return http.StatusNotFound, "base nar download error", err
		}
		return http.StatusInternalServerError, "base nar download error", err
	}
	defer baseClose()

	// if d.deltaSem.Acquire(r.Context(), 1) != nil {
	// 	return http.StatusInternalServerError, "canceled", nil
	// }
	// defer d.deltaSem.Release(1)

	// TODO: consider a quick check on delta-bility before we do it for real,
	// to save computation/bandwidth

	mpw := multipart.NewWriter(w)
	defer func() {
		if closeErr := mpw.Close(); closeErr != nil && retErr == nil {
			retErr = closeErr
		}
	}()

	w.Header().Set("Content-Type", mpw.FormDataContentType())

	// write our header
	var h differHeader
	h.Algo = algo.Name()
	if err := writeJsonField(mpw, differHeaderName, h); err != nil {
		return http.StatusInternalServerError, "multipart write header", err
	}

	// write body
	bw, err := mpw.CreateFormFile(differBodyName, "delta")

	stats, algoErr := algo.Create(r.Context(), CreateArgs{
		Base:        baseNar,
		BaseSize:    baseSize,
		Request:     reqNar,
		RequestSize: reqSize,
		Output:      bw,
	})

	// make sure downloads finished cleanly
	joinErr := errors.Join(reqClose(), baseClose(), algoErr)

	var t differTrailer

	if joinErr != nil {
		t.Ok = false
		t.Error = joinErr.Error()
	} else {
		t.Ok = true
		t.Stats = stats
		t.Stats.BaseSize = int(baseSize)
	}

	// write trailer
	err = writeJsonField(mpw, differTrailerName, t)
	if err != nil {
		return http.StatusInternalServerError, "multipart write trailer", err
	}

	return 0, t.Stats.String(), algoErr
}

func (d *differServer) streamNar(
	upstream, reqName, narPath string,
	size int64,
	narFilter readerFilter,
) (r io.Reader, sizeOut int64, close func() error, retErr error) {
	fileHash := path.Base(narPath)
	compression := path.Ext(fileHash)

	start := time.Now()
	u := url.URL{Scheme: "http", Host: upstream, Path: "/" + narPath}
	res, err := http.Get(u.String())
	if err != nil {
		log.Print("download http error: ", err, " for ", u.String())
		return nil, 0, nil, err
	}
	if res.StatusCode != http.StatusOK {
		log.Print("download http status: ", res.Status, " for ", u.String())
		res.Body.Close()
		return nil, 0, nil, fmt.Errorf("http error %s", res.Status)
	}

	var decompress *exec.Cmd
	switch compression {
	case "", "none":
		decompress = exec.Command(catBin)
	case ".xz":
		decompress = exec.Command(xzBin, "-d")
	case ".zst":
		decompress = exec.Command(zstdBin, "-d")
	default:
		return nil, 0, nil, fmt.Errorf("unknown compression %q", compression)
	}
	cr := countReader{r: res.Body}
	decompress.Stdin = &cr
	r, err = decompress.StdoutPipe()
	if err != nil {
		return nil, 0, nil, err
	}
	if narFilter != nil {
		r = narFilter(r)
	}
	decompress.Stderr = os.Stderr
	if err = decompress.Start(); err != nil {
		log.Print("download decompress start error: ", err)
		return nil, 0, nil, err
	}
	// TODO: can we wrap r in a ReadCloser such that Close does this but it's still actually
	// an *os.File for efficient piping?
	close = func() error {
		if decompress.ProcessState != nil {
			return nil // already called
		}
		if err = decompress.Wait(); err != nil {
			log.Print("download decompress error: ", err)
			return err
		}
		elapsed := time.Since(start)
		ps := decompress.ProcessState
		log.Printf("downloaded %s [%d bytes] in %s [decmp %s user, %s sys]: %.3f MB/s",
			reqName, cr.c, elapsed, ps.UserTime(), ps.SystemTime(),
			float64(size)/elapsed.Seconds()/1e6,
		)
		res.Body.Close()
		return nil
	}
	// FIXME FIXME FIXME: size is wrong if we used narFilter!
	sizeOut = size
	return
}

func (d *differServer) streamNarFromInfo(
	upstream, storePathHash string,
	narFilter readerFilter,
) (io.Reader, int64, func() error, error) {
	u := url.URL{
		Scheme: "http",
		Host:   upstream,
		Path:   "/" + storePathHash + ".narinfo",
	}
	us := u.String()
	res, err := http.Get(us)
	if err != nil {
		return nil, 0, nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusNotFound {
			return nil, 0, nil, errNotFound
		}
		return nil, 0, nil, fmt.Errorf("http error %s", res.Status)
	}
	ni, err := narinfo.Parse(res.Body)
	if err != nil {
		return nil, 0, nil, err
	}
	return d.streamNar(
		upstream,
		ni.StorePath[44:],
		ni.URL,
		int64(ni.NarSize),
		narFilter,
	)
}

func writeJsonField(mpw *multipart.Writer, name string, v any) error {
	w, err := mpw.CreateFormField(name)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(v)
}

func getNarFilter(cfg *config, req *differRequest) (readerFilter, readerFilter) {
	switch req.NarFilter {
	case narFilterExpandV2:
		opts := cfgToNarExpanderOptions(cfg)
		ex := func(r io.Reader) io.Reader { return ExpandNar(r, opts) }
		cl := func(r io.Reader) io.Reader { return CollapseNar(r, opts) }
		return ex, cl
	default:
		return nil, nil
	}
}
