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
		stats   *DiffStats
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

func (s *subst) getCacheInfo(w http.ResponseWriter, r *http.Request) (int, string, error) {
	if r.Method != "GET" {
		return http.StatusMethodNotAllowed, "", nil
	}
	w.Header().Add("Content-Type", "text/x-nix-cache-info")
	fmt.Fprintf(w, "StoreDir: /nix/store\nWantMassQuery: 0\nPriority: 10\n")
	return 0, "", nil
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

func (s *subst) getLog(w http.ResponseWriter, r *http.Request) (int, string, error) {
	return http.StatusNotFound, "", nil
}

func (s *subst) getNar(w http.ResponseWriter, r *http.Request) (int, string, error) {
	if r.Method != "GET" {
		return http.StatusMethodNotAllowed, "", nil
	}

	dir, narbasename := path.Split(r.URL.Path)
	if dir != "/nar/" || !strings.HasSuffix(narbasename, ".nar") {
		return http.StatusNotFound, "", nil
	}

	recent := s.getRecent(narbasename)
	if recent == nil {
		return http.StatusNotFound, "no recent found", nil
	}

	if s.nsem.Acquire(r.Context(), 1) != nil {
		return http.StatusInternalServerError, "canceled", nil
	}
	defer s.nsem.Release(1)

	return s.getNarCommon(r.Context(), recent, w)
}

func (s *subst) getNarCommon(ctx context.Context, recent *recent, w io.Writer) (int, string, error) {
	// make diff request
	buf, err := json.Marshal(recent.request)
	if err != nil {
		return http.StatusInternalServerError, "json marshal error", err
	}
	u := makeDifferUrl(s.cfg.Differ)
	postReq, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(buf))
	if err != nil {
		return http.StatusInternalServerError, "create req", err
	}
	postReq.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(postReq)
	if err != nil {
		return http.StatusInternalServerError, "differ http error", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		// TODO: on some/most errors, fall back to proxying from upstream cache directly
		return res.StatusCode, "differ http status", errors.New(res.Status)
	}

	// parse multipart
	boundary, err := getBoundary(res)
	if err != nil {
		return http.StatusInternalServerError, "parse multipart", err
	}
	mpr := multipart.NewReader(res.Body, boundary)

	// read header
	hr, err := mpr.NextPart()
	var h differHeader
	if err != nil {
		return http.StatusInternalServerError, "parse multipart header", err
	} else if hr.FormName() != differHeaderName {
		return http.StatusInternalServerError, "parse multipart header wrong name", nil
	} else if err = json.NewDecoder(hr).Decode(&h); err != nil {
		return http.StatusInternalServerError, "parse multipart header json", err
	}

	algo := getAlgo(h.Algo)
	if algo == nil {
		return http.StatusInternalServerError, "unknown algo", nil
	}

	// set up for reading body
	br, err := mpr.NextRawPart()
	if err != nil {
		return http.StatusInternalServerError, "parse multipart body", err
	} else if br.FormName() != differBodyName {
		return http.StatusInternalServerError, "parse multipart body wrong name", nil
	}

	// get base nar

	procCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	writeNar := exec.CommandContext(procCtx, nixBin+"-store", "--dump", recent.request.BaseStorePath)
	var basePipe io.Reader
	basePipe, err = writeNar.StdoutPipe()
	if err != nil {
		return http.StatusInternalServerError, "pipe error", err
	}
	writeNar.Stderr = os.Stderr
	err = writeNar.Start()
	if err != nil {
		return http.StatusInternalServerError, "base dump error", err
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
	expandStats, err := algo.Expand(procCtx, ExpandArgs{
		Base:   basePipe,
		Delta:  br,
		Output: output,
	})
	if err != nil {
		return http.StatusInternalServerError, "diff algo error", err
	}

	filterErr := <-filterErrCh

	// this should also be done now
	if err = writeNar.Wait(); err != nil {
		return http.StatusInternalServerError, "base dump error", err
	} else if filterErr != nil {
		return http.StatusInternalServerError, "nar filter error", filterErr
	}

	// read trailer
	tr, err := mpr.NextPart()
	var t differTrailer
	if err != nil {
		return http.StatusInternalServerError, "parse multipart trailer", err
	} else if tr.FormName() != differTrailerName {
		return http.StatusInternalServerError, "parse multipart trailer wrong name", nil
	} else if err = json.NewDecoder(tr).Decode(&t); err != nil {
		return http.StatusInternalServerError, "parse multipart trailer json", err
	} else if _, err = mpr.NextPart(); err != io.EOF {
		return http.StatusInternalServerError, "parse multipart trailing parts", nil
	}

	if !t.Ok {
		return http.StatusInternalServerError, "", errors.New("trailer ok false")
	}

	recent.stats = t.Stats.nonnil()
	recent.stats.ExpTotalMs = expandStats.ExpTotalMs
	recent.stats.ExpUserMs = expandStats.ExpUserMs
	recent.stats.ExpSysMs = expandStats.ExpSysMs

	s.writeAnalytics(AnRecord{
		D: &AnDiff{
			Id:        recent.id,
			DiffStats: recent.stats,
		},
	})

	return 0, recent.stats.String(), nil
}

func (s *subst) getNarInfo(w http.ResponseWriter, r *http.Request) (int, string, error) {
	if r.Method != "GET" && r.Method != "HEAD" {
		return http.StatusMethodNotAllowed, "", nil
	}
	m := reInfo.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return http.StatusNotFound, "", nil
	}
	hash, tp := m[1], m[2]
	head := r.Method == "HEAD"

	// don't support listings
	if tp == "ls" {
		return http.StatusNotFound, "", nil
	}

	if s.nisem.Acquire(r.Context(), 1) != nil {
		return http.StatusInternalServerError, "canceled", nil
	}
	defer s.nisem.Release(1)

	_, status, msg, err := s.getNarInfoCommon(r.Context(), hash, head, w)
	return status, msg, err
}

func (s *subst) getNarInfoCommon(
	ctx context.Context,
	hash string,
	head bool,
	w http.ResponseWriter,
) (*recent, int, string, error) {
	reqid := newId()

	// check upstream
	res, err := s.makeUpstreamRequest(ctx, hash, head)
	if err != nil {
		return nil, http.StatusInternalServerError, "upstream http error", err
	}
	defer res.Body.Close()
	if head {
		return nil, res.StatusCode, "", nil
	}
	if isNotFound(res.StatusCode) {
		s.writeAnalytics(AnRecord{
			R: &AnRequest{
				Id:           reqid,
				ReqStorePath: hash,
				Failed:       failedNotFound,
			},
		})
		return nil, res.StatusCode, "upstream not found", errors.New(res.Status)
	} else if res.StatusCode != http.StatusOK {
		return nil, res.StatusCode, "upstream http status", errors.New(res.Status)
	}
	ni, err := narinfo.Parse(res.Body)
	if err != nil {
		return nil, http.StatusInternalServerError, "narinfo parse error", err
	}
	np, err := nixpath.FromString(ni.StorePath)
	if err != nil {
		return nil, http.StatusInternalServerError, "nixpath parse error", err
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
		msg := fmt.Sprintf("%s is too %s (%d)", np.Name, code[3:], ni.FileSize)
		return nil, http.StatusNotFound, msg, nil
	}

	// see if we have any reasonable base
	base, narFilter, err := s.catalog.findBase(ni, np.Name)
	if err != nil || base[11:43] == hash {
		code := failedNoBase
		if err == nil && base[11:43] == hash {
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
		return nil, http.StatusNotFound, "", err
	}

	// new url for uncompressed nar
	newUrl := "nar/" + strings.TrimPrefix(ni.NarHash.NixString(), "sha256:") + ".nar"

	// record this for nar serving
	recent := &recent{
		id: reqid,
		request: differRequest{
			ReqNarPath:    ni.URL,
			BaseStorePath: base,
			AcceptAlgos:   strings.Split(s.cfg.DiffAlgo, ","),
			NarFilter:     narFilter,
			Upstream:      s.cfg.Upstream,

			ReqNarSize: int64(ni.NarSize),
			ReqName:    np.Name,
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
			BaseStorePath: base[len(nixpath.StoreDir)+1:],
			NarSize:       ni.NarSize,
			FileSize:      origFileSize,
			DifferRequest: &recent.request,
		},
	})

	return recent, 0, "", nil
}

func (s *subst) request(ctx context.Context, req string) (*DiffStats, error) {
	// req should be store name (without /nix/store)
	hash, _, _ := strings.Cut(req, "-")

	recent, status, msg, err := s.getNarInfoCommon(ctx, hash, false, nil)
	if err != nil || status != 0 {
		return nil, fmt.Errorf("get narinfo %s: %d %s: %w", req, status, msg, err)
	}
	out := &countWriter{w: io.Discard}
	status, msg, err = s.getNarCommon(ctx, recent, out)
	if err != nil || status != 0 {
		return nil, fmt.Errorf("get nar %s: %d %s: %w", req, status, msg, err)
	}
	// fmt.Printf("%s: %d bytes\n", req, out.c)
	return recent.stats, nil
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
