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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nix-community/go-nix/pkg/narinfo"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
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
		BaseNarSize int64  `json:"baseNarSize"` // size of base nar
		ReqNarSize  int64  `json:"reqNarSize"`  // size of requested nar (used for resource control)
		ReqName     string `json:"reqName"`     // requested (name only, no hash) (used for log)
	}

	differServer struct {
		cfg      *config
		diskSem  *semaphore.Weighted
		dlSem    *semaphore.Weighted
		deltaSem *semaphore.Weighted
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
	// roughly, each download will use some network plus an xz process,
	// and each delta will use an xdelta3/zstd process.
	// so effectively this will allow about 2×cpus processes to run.
	concurrency := int64(runtime.NumCPU())
	return &differServer{
		cfg:      cfg,
		diskSem:  semaphore.NewWeighted(getTempDirFreeBytes()),
		dlSem:    semaphore.NewWeighted(concurrency),
		deltaSem: semaphore.NewWeighted(concurrency),
	}
}

func (d *differServer) getHander() http.Handler {
	h := http.NewServeMux()
	h.HandleFunc(differPath, fw(d.differ, nil))
	return h
}

func (d *differServer) serve() error {
	srv := &http.Server{
		Addr:    d.cfg.DifferBind,
		Handler: d.getHander(),
	}
	return srv.ListenAndServe()
}

func (d *differServer) differ(w http.ResponseWriter, r *http.Request) (retErr error) {
	if r.Method != "POST" {
		return fwErr(http.StatusMethodNotAllowed, "")
	}

	var req differRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fwErr(http.StatusBadRequest, "json decode error: %w", err)
	}
	if req.Upstream == "" {
		req.Upstream = d.cfg.Upstream
	}
	// TODO: should we do this?
	// Will need to sign requests now, see https://discourse.nixos.org/t/34697
	// if req.Upstream == "cache.nixos.org" && os.Getenv("AWS_REGION") == "us-east-1" {
	// 	// If we're in us-east-1, prefer S3 directly since it's free.
	// 	req.Upstream = "nix-cache.s3.amazonaws.com"
	// }

	// TODO: pick algo based on size or other properties?
	algo := pickAlgo(req.AcceptAlgos)
	if algo == nil {
		return fwErr(http.StatusBadRequest, "unknown algo %q", req.AcceptAlgos)
	}

	// times two because we need base + requested and we expect them to be about the same size
	size := req.ReqNarSize * 2
	if err := d.diskSem.Acquire(r.Context(), size); err != nil {
		return fwErr(http.StatusInsufficientStorage, "disk semaphore: %w", err)
	}
	defer d.diskSem.Release(size)

	// download base + req nar
	var baseNar, reqNar string
	var g errgroup.Group
	var baseSize int
	expFilter, _ := getNarFilter(d.cfg, &req)

	g.Go(func() error {
		if err := d.dlSem.Acquire(r.Context(), 1); err != nil {
			return err
		}
		defer d.dlSem.Release(1)

		var err error
		reqNar, err = d.downloadNar(req.Upstream, req.ReqName, req.ReqNarPath, expFilter)
		return err
	})
	g.Go(func() error {
		if err := d.dlSem.Acquire(r.Context(), 1); err != nil {
			return err
		}
		defer d.dlSem.Release(1)

		var err error
		hash, _, _ := strings.Cut(path.Base(req.BaseStorePath), "-")
		baseNar, err = d.downloadNarFromInfo(req.Upstream, hash, expFilter)
		if err == nil {
			if st, e := os.Stat(baseNar); e == nil {
				baseSize = int(st.Size())
			}
		}
		return err
	})

	err := g.Wait()
	defer os.Remove(baseNar)
	defer os.Remove(reqNar)

	if err != nil {
		if err == errNotFound {
			return fwErr(http.StatusNotFound, "nar download error: %w", err)
		}
		return fwErr(http.StatusInternalServerError, "nar download error: %w", err)
	}

	if d.deltaSem.Acquire(r.Context(), 1) != nil {
		return fwErr(http.StatusInternalServerError, "canceled")
	}
	defer d.deltaSem.Release(1)

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
		return fwErr(http.StatusInternalServerError, "multipart write header: %w", err)
	}

	// write body
	bw, err := mpw.CreateFormFile(differBodyName, "delta")

	stats, algoErr := algo.Create(r.Context(), CreateArgs{
		Base:    baseNar,
		Request: reqNar,
		Output:  bw,
	})

	var t differTrailer

	if algoErr != nil {
		t.Ok = false
		t.Error = algoErr.Error()
	} else {
		t.Ok = true
		t.Stats = stats
		t.Stats.BaseSize = baseSize
	}

	// write trailer
	err = writeJsonField(mpw, differTrailerName, t)
	if err != nil {
		return fwErr(http.StatusInternalServerError, "multipart write trailer: %w", err)
	}

	if algoErr != nil {
		return fwErr(http.StatusInternalServerError, "algo error: %w", algoErr)
	}

	// return stats as zero "error" for the log
	return fwErr(0, "%s", t.Stats.String())
}

func (d *differServer) downloadNar(upstream, reqName, narPath string, narFilter readerFilter) (retPath string, retErr error) {
	fileHash := path.Base(narPath)
	compression := path.Ext(fileHash)
	fileHash = strings.TrimSuffix(fileHash, compression)

	start := time.Now()
	u := url.URL{Scheme: "http", Host: upstream, Path: "/" + narPath}
	res, err := http.Get(u.String())
	if err != nil {
		log.Print("download http error: ", err, " for ", u.String())
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		log.Print("download http status: ", res.Status, " for ", u.String())
		return "", fmt.Errorf("http error %s", res.Status)
	}

	f, err := os.CreateTemp("", "nar")
	name := f.Name()
	defer func() {
		if retErr != nil {
			os.Remove(name)
		}
	}()
	defer f.Close()

	var decompress *exec.Cmd
	switch compression {
	case "", "none":
		decompress = exec.Command(catBin)
	case ".xz":
		decompress = exec.Command(xzBin, "-d")
	case ".zst":
		decompress = exec.Command(zstdBin, "-d")
	default:
		return "", fmt.Errorf("unknown compression %q", compression)
	}
	decompress.Stdin = res.Body
	filterErrCh := make(chan error, 1)
	if narFilter == nil {
		decompress.Stdout = f
		filterErrCh <- nil
	} else {
		pr, err := decompress.StdoutPipe()
		if err != nil {
			return "", err
		}
		expanded := narFilter(pr)
		go func() { filterErrCh <- ioCopy(f, expanded, nil, -1) }()
	}
	decompress.Stderr = os.Stderr
	if err = decompress.Start(); err != nil {
		log.Print("download decompress start error: ", err)
		return "", err
	}
	filterErr := <-filterErrCh
	if err = decompress.Wait(); err != nil {
		log.Print("download decompress error: ", err)
		return "", err
	}
	if filterErr != nil {
		log.Print("download filter error: ", err)
		return "", err
	}
	var size int64
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}

	elapsed := time.Since(start)
	ps := decompress.ProcessState
	log.Printf("downloaded %s [%d bytes] in %s [decmp %s user, %s sys]: %.3f MB/s",
		reqName, size, elapsed, ps.UserTime(), ps.SystemTime(),
		float64(size)/elapsed.Seconds()/1e6,
	)
	return name, nil
}

func (d *differServer) downloadNarFromInfo(upstream, storePathHash string, narFilter readerFilter) (string, error) {
	u := url.URL{
		Scheme: "http",
		Host:   upstream,
		Path:   "/" + storePathHash + ".narinfo",
	}
	us := u.String()
	res, err := http.Get(us)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusNotFound {
			return "", errNotFound
		}
		return "", fmt.Errorf("http error %s", res.Status)
	}
	ni, err := narinfo.Parse(res.Body)
	if err != nil {
		return "", err
	}
	return d.downloadNar(upstream, ni.StorePath[44:], ni.URL, narFilter)
}

func writeJsonField(mpw *multipart.Writer, name string, v any) error {
	w, err := mpw.CreateFormField(name)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(v)
}

func getTempDirFreeBytes() int64 {
	t := os.TempDir()
	var st syscall.Statfs_t
	if err := syscall.Statfs(t, &st); err != nil {
		panic(err)
	}
	return int64(st.Bfree) * st.Bsize * 9 / 10
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
