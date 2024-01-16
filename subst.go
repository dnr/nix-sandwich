package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/golang/groupcache/lru"
	"github.com/nix-community/go-nix/pkg/narinfo"
	"github.com/nix-community/go-nix/pkg/nixbase32"
	"github.com/nix-community/go-nix/pkg/nixpath"
	"golang.org/x/sync/semaphore"
)

var (
	reInfo = regexp.MustCompile(`^/([` + nixbase32.Alphabet + `]+)\.(narinfo|ls)$`)
)

type (
	subst struct {
		cfg     *config
		catalog *catalog
		nisem   *semaphore.Weighted
		nsem    *semaphore.Weighted
		lastReq atomic.Int64

		analytics *os.File

		recents     *lru.Cache
		recentsLock sync.Mutex
	}

	recent struct {
		id      string
		request differRequest
	}

	diffSource struct {
		body   io.Reader
		finish func() error
		algo   DiffAlgo
		cached string
	}
)

func newLocalSubstituter(cfg *config, catalog *catalog) *subst {
	return &subst{
		cfg:       cfg,
		catalog:   catalog,
		analytics: openAnalyticsLog(cfg.AnalyticsFile),
		recents:   lru.New(10000),
		nisem:     semaphore.NewWeighted(40),
		nsem:      semaphore.NewWeighted(20),
	}
}

func (s *subst) serve() error {
	h := http.NewServeMux()
	h.HandleFunc("/nix-cache-info", fw(s.getCacheInfo, s.alive))
	h.HandleFunc("/log/", fw(s.getLog, s.alive))
	h.HandleFunc("/nar/", fw(s.getNar, s.alive))
	h.HandleFunc("/", fw(s.getNarInfo, s.alive))

	listeners, err := activation.Listeners()
	if err != nil {
		panic(err)
	}
	if len(listeners) == 0 {
		// not using socket activation
		return http.ListenAndServe(s.cfg.SubstituterBind, h)
	}

	if s.cfg.SubstIdleTime > 0 {
		go s.exitOnIdle()
	}
	daemon.SdNotify(true, daemon.SdNotifyReady)
	return http.Serve(listeners[0], h)
}

func (s *subst) exitOnIdle() {
	for range time.NewTicker(time.Minute).C {
		if time.Since(time.Unix(s.lastReq.Load(), 0)) > s.cfg.SubstIdleTime {
			os.Exit(0)
		}
	}
}

func (s *subst) alive() {
	s.lastReq.Store(time.Now().Unix())
}

func (s *subst) getCacheInfo(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "GET" {
		return fwErr(http.StatusMethodNotAllowed, "")
	}
	w.Header().Add("Content-Type", "text/x-nix-cache-info")
	fmt.Fprintf(w, "StoreDir: /nix/store\nWantMassQuery: 0\nPriority: 10\n")
	return nil
}

func (s *subst) getRecent(narbasename string) *recent {
	s.recentsLock.Lock()
	defer s.recentsLock.Unlock()
	v, ok := s.recents.Get(narbasename)
	if !ok {
		return nil
	}
	return v.(*recent)
}

func (s *subst) putRecent(narbasename string, r *recent) {
	s.recentsLock.Lock()
	s.recents.Add(narbasename, r)
	s.recentsLock.Unlock()
}

func (s *subst) getLog(w http.ResponseWriter, r *http.Request) error {
	return fwErr(http.StatusNotFound, "")
}

func (s *subst) getNar(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "GET" {
		return fwErr(http.StatusMethodNotAllowed, "")
	}

	dir, narbasename := path.Split(r.URL.Path)
	if dir != "/nar/" || !strings.HasSuffix(narbasename, ".nar") {
		return fwErr(http.StatusNotFound, "")
	}

	recent := s.getRecent(narbasename)
	if recent == nil {
		return fwErr(http.StatusNotFound, "no recent found")
	}

	if s.nsem.Acquire(r.Context(), 1) != nil {
		return fwErr(http.StatusInternalServerError, "canceled")
	}
	defer s.nsem.Release(1)

	_, _, err := s.getNarCommon(r.Context(), recent, w)
	return err
}

func (s *subst) getDiff(ctx context.Context, recent *recent) (dr diffSource, retErr error) {
	cached := "C?"
	// check cache first
	if len(s.cfg.CacheReadURL) > 0 {
		// first algo only
		algo := pickAlgo(recent.request.AcceptAlgos)
		if algo == nil {
			return diffSource{}, fmt.Errorf("unknown algo")
		}

		key := cacheKey(&recent.request, algo.Name())
		u, err := url.Parse(s.cfg.CacheReadURL)
		if err != nil {
			panic(err)
		}
		u.Path = path.Join(u.Path, key)

		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return diffSource{}, err
		}
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			if res.StatusCode == http.StatusOK {
				return diffSource{
					body:   res.Body,
					finish: res.Body.Close,
					algo:   algo,
					cached: "C+",
				}, nil
			}
			// TODO: retry on certain status codes (503?)
			cached = "C-"
			// http success but no hit, ignore body and fall through to differ
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}

	// make diff request
	buf, err := json.Marshal(recent.request)
	if err != nil {
		return diffSource{}, fmt.Errorf("json marshal error: %w", err)
	}
	u := makeDifferUrl(s.cfg.Differ)
	postReq, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(buf))
	if err != nil {
		return diffSource{}, fmt.Errorf("create req: %w", err)
	}
	postReq.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(postReq)
	if err != nil {
		return diffSource{}, fmt.Errorf("differ http error: %w", err)
	}
	defer func() {
		if retErr != nil {
			res.Body.Close()
		}
	}()

	if res.StatusCode != http.StatusOK {
		// TODO: on some/most errors, fall back to proxying from upstream cache directly
		return diffSource{}, fmt.Errorf("differ http status %s", res.Status)
	}

	// parse multipart
	boundary, err := getBoundary(res)
	if err != nil {
		return diffSource{}, fmt.Errorf("parse multipart: %w", err)
	}
	mpr := multipart.NewReader(res.Body, boundary)

	// read header
	hr, err := mpr.NextPart()
	var h differHeader
	if err != nil {
		return diffSource{}, fmt.Errorf("parse multipart header: %w", err)
	} else if hr.FormName() != differHeaderName {
		return diffSource{}, fmt.Errorf("parse multipart header wrong name")
	} else if err = json.NewDecoder(hr).Decode(&h); err != nil {
		return diffSource{}, fmt.Errorf("parse multipart header json: %w", err)
	}

	algo := getAlgo(h.Algo)
	if algo == nil {
		return diffSource{}, fmt.Errorf("unknown algo %q", h.Algo)
	}

	// set up for reading body
	br, err := mpr.NextRawPart()
	if err != nil {
		return diffSource{}, fmt.Errorf("parse multipart body: %w", err)
	} else if br.FormName() != differBodyName {
		return diffSource{}, fmt.Errorf("parse multipart body wrong name")
	}

	finish := func() error {
		defer res.Body.Close()
		tr, err := mpr.NextPart()
		var t differTrailer
		if err != nil {
			return fmt.Errorf("parse multipart trailer: %w", err)
		} else if tr.FormName() != differTrailerName {
			return fmt.Errorf("parse multipart trailer wrong name")
		} else if err = json.NewDecoder(tr).Decode(&t); err != nil {
			return fmt.Errorf("parse multipart trailer json: %w", err)
		} else if _, err = mpr.NextPart(); err != io.EOF {
			return fmt.Errorf("parse multipart trailing parts")
		} else if !t.Ok {
			return fmt.Errorf("differ error: %s", t.Error)
		}
		return nil
	}

	return diffSource{
		body:   br,
		finish: finish,
		algo:   algo,
		cached: cached,
	}, nil
}

func (s *subst) getNarCommon(ctx context.Context, recent *recent, w io.Writer) (*DiffStats, string, error) {
	diff, err := s.getDiff(ctx, recent)
	if err != nil {
		return nil, "", fwErrE(http.StatusInternalServerError, err)
	}
	diffReader := countReader{r: diff.body}

	// get base nar

	procCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	writeNar := exec.CommandContext(procCtx, nixBin+"-store", "--dump", recent.request.BaseStorePath)
	var basePipe io.Reader
	basePipe, err = writeNar.StdoutPipe()
	if err != nil {
		return nil, "", fwErr(http.StatusInternalServerError, "pipe error: %w", err)
	}
	writeNar.Stderr = os.Stderr
	err = writeNar.Start()
	if err != nil {
		return nil, "", fwErr(http.StatusInternalServerError, "base dump error: %w", err)
	}
	defer writeNar.Wait()

	expFilter, colFilter := getNarFilter(s.cfg, &recent.request)
	if expFilter != nil {
		basePipe = expFilter(basePipe)
	}
	output := w
	filterErrCh := make(chan error, 1)
	if colFilter == nil {
		filterErrCh <- nil
	} else {
		filtR, filtW := io.Pipe()
		output = filtW
		go func() { filterErrCh <- ioCopy(w, colFilter(filtR), nil, -1) }()
	}

	// run algo
	expandStats, err := diff.algo.Expand(procCtx, ExpandArgs{
		Base:   basePipe,
		Delta:  &diffReader,
		Output: output,
	})
	if err != nil {
		return nil, "", fwErr(http.StatusInternalServerError, "diff algo error: %w", err)
	}

	filterErr := <-filterErrCh

	// this should also be done now
	if err = writeNar.Wait(); err != nil {
		return nil, "", fwErr(http.StatusInternalServerError, "base dump error: %w", err)
	} else if filterErr != nil {
		return nil, "", fwErr(http.StatusInternalServerError, "nar filter error: %w", filterErr)
	}

	// read trailer
	err = diff.finish()
	if err != nil {
		return nil, "", fwErrE(http.StatusInternalServerError, err)
	}

	stats := &DiffStats{
		BaseSize: int(recent.request.BaseNarSize),
		DiffSize: diffReader.c,
		NarSize:  int(recent.request.ReqNarSize),
		Algo:     diff.algo.Name(),
		Level:    0, // TODO: get level
		// TODO: get cmp stats in here
		ExpTotalMs: expandStats.ExpTotalMs,
		ExpUserMs:  expandStats.ExpUserMs,
		ExpSysMs:   expandStats.ExpSysMs,
	}

	s.writeAnalytics(AnRecord{
		D: &AnDiff{
			Id:        recent.id,
			DiffStats: stats,
		},
	})

	// return stats as zero "error" for the log
	return stats, diff.cached, fwErr(0, "%s %s", diff.cached, stats.String())
}

func (s *subst) getNarInfo(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "GET" && r.Method != "HEAD" {
		return fwErr(http.StatusMethodNotAllowed, "")
	}
	m := reInfo.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return fwErr(http.StatusNotFound, "")
	}
	hash, tp := m[1], m[2]
	head := r.Method == "HEAD"

	// don't support listings
	if tp == "ls" {
		return fwErr(http.StatusNotFound, "")
	}

	if s.nisem.Acquire(r.Context(), 1) != nil {
		return fwErr(http.StatusInternalServerError, "canceled")
	}
	defer s.nisem.Release(1)

	_, err := s.getNarInfoCommon(r.Context(), hash, head, w)
	return err
}

func (s *subst) getNarInfoCommon(
	ctx context.Context,
	hash string,
	head bool,
	w http.ResponseWriter,
) (*recent, error) {
	reqid := newId()

	// check upstream
	res, err := s.makeUpstreamRequest(ctx, hash, head)
	if err != nil {
		return nil, fwErr(http.StatusInternalServerError, "upstream http error: %w", err)
	}
	defer res.Body.Close()
	if head {
		return nil, fwErr(res.StatusCode, "")
	}
	if isNotFound(res.StatusCode) {
		s.writeAnalytics(AnRecord{
			R: &AnRequest{
				Id:           reqid,
				ReqStorePath: hash,
				Failed:       failedNotFound,
			},
		})
		return nil, fwErr(res.StatusCode, "upstream not found: %s", res.Status)
	} else if res.StatusCode != http.StatusOK {
		return nil, fwErr(res.StatusCode, "upstream http status: %s", res.Status)
	}
	ni, err := narinfo.Parse(res.Body)
	if err != nil {
		return nil, fwErr(http.StatusInternalServerError, "narinfo parse error: %w", err)
	}
	np, err := nixpath.FromString(ni.StorePath)
	if err != nil {
		return nil, fwErr(http.StatusInternalServerError, "nixpath parse error: %w", err)
	}
	if int(ni.FileSize) < s.cfg.MinFileSize || int(ni.FileSize) > s.cfg.MaxFileSize || int(ni.NarSize) > s.cfg.MaxNarSize {
		code := failedTooSmall
		if int(ni.FileSize) > s.cfg.MaxFileSize || int(ni.NarSize) > s.cfg.MaxNarSize {
			code = failedTooBig
		}
		s.writeAnalytics(AnRecord{
			R: &AnRequest{
				Id:           reqid,
				ReqStorePath: ni.StorePath[len(nixpath.StoreDir)+1:],
				NarSize:      ni.NarSize,
				FileSize:     ni.FileSize,
				Failed:       code,
			},
		})
		// too small or too big, pretend we don't have it
		return nil, fwErr(http.StatusNotFound, "%s is too %s (%d)", np.Name, code[3:], ni.FileSize)
	}

	// see if we have any reasonable base
	base, err := s.catalog.findBase(ni, np.Name)
	if err != nil || base.storePath[11:43] == hash {
		code := failedNoBase
		if err == nil && base.storePath[11:43] == hash {
			// only would happen in simulation, real nix wouldn't request this
			code = failedIdentical
			err = errors.New("identical")
		}
		s.writeAnalytics(AnRecord{
			R: &AnRequest{
				Id:           reqid,
				ReqStorePath: ni.StorePath[len(nixpath.StoreDir)+1:],
				NarSize:      ni.NarSize,
				FileSize:     ni.FileSize,
				Failed:       code,
			},
		})
		return nil, fwErrE(http.StatusNotFound, err)
	}

	// new url for uncompressed nar
	newUrl := "nar/" + strings.TrimPrefix(ni.NarHash.NixString(), "sha256:") + ".nar"

	// record this for nar serving
	recent := &recent{
		id: reqid,
		request: differRequest{
			ReqNarPath:    ni.URL,
			BaseStorePath: base.storePath,
			AcceptAlgos:   strings.Split(s.cfg.DiffAlgo, ","),
			NarFilter:     base.narFilter,
			Upstream:      s.cfg.Upstream,

			BaseNarSize: base.narSize,
			ReqNarSize:  int64(ni.NarSize),
			ReqName:     np.Name,
		},
	}
	s.putRecent(path.Base(newUrl), recent)

	// set up narinfo with new path
	origFileSize := ni.FileSize
	ni.URL = newUrl
	ni.Compression = "none"
	ni.FileHash = ni.NarHash
	ni.FileSize = ni.NarSize

	if w != nil {
		w.Header().Add("Content-Type", ni.ContentType())
		w.Write([]byte(ni.String()))
	}

	s.writeAnalytics(AnRecord{
		R: &AnRequest{
			Id:            reqid,
			ReqStorePath:  ni.StorePath[len(nixpath.StoreDir)+1:],
			BaseStorePath: base.storePath[len(nixpath.StoreDir)+1:],
			NarSize:       ni.NarSize,
			FileSize:      origFileSize,
			DifferRequest: &recent.request,
		},
	})

	return recent, nil
}

func (s *subst) request(ctx context.Context, req string) (*DiffStats, string, error) {
	// req should be store name (without /nix/store)
	hash, _, _ := strings.Cut(req, "-")

	recent, err := s.getNarInfoCommon(ctx, hash, false, nil)
	if err != nil {
		if ewc := err.(*errWithStatus); ewc != nil && ewc.status > 0 {
			return nil, "", fmt.Errorf("get narinfo %s: %d %w", req, ewc.status, ewc.error)
		}
	}
	out := &countWriter{w: io.Discard}
	stats, cached, err := s.getNarCommon(ctx, recent, out)
	if ewc := err.(*errWithStatus); ewc != nil && ewc.status > 0 {
		return nil, "", fmt.Errorf("get nar %s: %d %w", req, ewc.status, ewc.error)
	}
	// fmt.Printf("%s: %d bytes\n", req, out.c)
	return stats, cached, nil
}

func (s *subst) makeUpstreamRequest(ctx context.Context, storeHash string, head bool) (*http.Response, error) {
	u := url.URL{
		Scheme: "https",
		Host:   s.cfg.Upstream,
		Path:   "/" + storeHash + ".narinfo",
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	if head {
		req.Method = "HEAD"
	}
	return http.DefaultClient.Do(req)
}

func (s *subst) writeAnalytics(rec AnRecord) {
	if s.analytics == nil {
		return
	}
	rec.T = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	b = append(b, '\n')
	s.analytics.Write(b)
}

func openAnalyticsLog(name string) *os.File {
	if name == "" {
		return nil
	} else if name == "default" {
		base := "log"
		if d := os.Getenv("LOGS_DIRECTORY"); d != "" { // set by systemd
			base = d
		}
		_ = os.MkdirAll(base, 0755)
		now := time.Now().Format(time.RFC3339)
		name = fmt.Sprintf("%s/%s.jsonl", base, now)
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil
	}
	return f
}

func newId() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

func getBoundary(res *http.Response) (string, error) {
	mt, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil {
		return "", err
	} else if mt != "multipart/form-data" {
		return "", errors.New("wrong content-type")
	} else if b := params["boundary"]; b != "" {
		return b, nil
	} else {
		return "", errors.New("missing boundary")
	}
}

func isNotFound(code int) bool {
	return code == http.StatusNotFound ||
		code == http.StatusUnauthorized ||
		code == http.StatusForbidden
}

func makeDifferUrl(d string) string {
	if strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://") {
		u, err := url.Parse(d)
		if err != nil {
			panic(err)
		}
		u.Path = differPath
		return u.String()
	}
	u := url.URL{
		Scheme: "https",
		Host:   d,
		Path:   differPath,
	}
	return u.String()
}
