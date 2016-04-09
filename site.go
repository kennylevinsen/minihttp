package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func access(req *http.Request, status int) {
	var forwardAddr, userAgent string
	var exists bool
	if forwardAddr, exists = quickHeaderGetLast("X-Forwarded-For", req.Header); !exists {
		forwardAddr = "-"
	}
	if userAgent, exists = quickHeaderGet("User-Agent", req.Header); !exists {
		userAgent = "-"
	}
	log.Printf("[%s] (%s): %d \"%s %v %s\" \"%s\"", req.RemoteAddr, forwardAddr, status, req.Method, req.URL, req.Proto, userAgent)
}

type resource struct {
	body     []byte
	gzip     []byte
	cnttype  string
	cache    time.Duration
	loaded   time.Time
	hash     string
	path     string
	fromDisk bool
	config   *SiteConfig
}

// update updates all information about the resource, given a body and a path.
// Its use eases synthetic content creation.
func (r *resource) update() {
	var (
		cacheconf    = r.config.Cache
		compressconf = r.config.Compression
		ext          = path.Ext(r.path)
		mincompsize  = compressconf.MinSize
		mincompratio = compressconf.MinRatio
		buf          *bytes.Buffer
		gz           io.WriteCloser
	)

	if compressconf.MinSize == 0 {
		mincompsize = DefaultSiteConfig.Compression.MinSize
	}
	if compressconf.MinRatio == 0 {
		mincompratio = DefaultSiteConfig.Compression.MinRatio
	}

	// Evaluate cache time.
	if (!r.fromDisk && !cacheconf.NoCacheFromMem) ||
		(r.fromDisk && !cacheconf.NoCacheFromDisk) {

		if x, exists := cacheconf.CacheTimes[ext]; exists {
			r.cache = x.Duration
		} else {
			r.cache = cacheconf.DefaultCacheTime.Duration
		}
	}

	// Evaluate compression.
	if (r.fromDisk && compressconf.NoCompressFromDisk) ||
		(!r.fromDisk && compressconf.NoCompressFromMem) ||
		len(r.body) < mincompsize {
		goto compdone
	}

	for _, v := range compressconf.Blacklist {
		if ext == v {
			goto compdone
		}
	}

	buf = new(bytes.Buffer)
	gz = gzip.NewWriter(buf)
	gz.Write(r.body)
	gz.Close()
	r.gzip = buf.Bytes()

	if float64(len(r.gzip))*mincompratio >= float64(len(r.body)) {
		r.gzip = nil
	}

compdone:

	// Hash and wrap thing sup.
	h := sha256.Sum256(r.body)
	r.hash = hex.EncodeToString(h[:])
	r.cnttype = mime.TypeByExtension(ext)
	r.loaded = time.Now()
}

type site struct {
	sync.RWMutex
	http   map[string]*resource
	https  map[string]*resource
	config *SiteConfig
}

func (s *site) addResource(diskpath, sitepath string, http, https bool) error {
	// TODO(kl): Deduplicate resources by sha or diskpath!

	st, err := os.Stat(diskpath)
	if err != nil {
		return err
	}

	if st.IsDir() {
		diskpath = path.Join(diskpath, s.config.General.DefaultFile)
		if st, err := os.Stat(diskpath); err != nil || st.IsDir() {
			// We're here because the path addResource was called with was a
			// directory, and the directory either lacked the default file, or
			// the default file was a directory as well. Not being able to
			// associate a default file with a directory is not an error, so we
			// just skip the entry.
			return nil
		}
	}

	body, err := ioutil.ReadFile(diskpath)
	if err != nil {
		return err
	}

	res := &resource{
		path:   diskpath,
		config: s.config,
		body:   body,
	}

	res.update()

	if http {
		s.http[sitepath] = res
	}
	if https {
		s.https[sitepath] = res
	}

	return nil
}

func newSite(config *SiteConfig) *site {
	return &site{
		http:   make(map[string]*resource),
		https:  make(map[string]*resource),
		config: config,
	}
}

// sitelist manages a set of sites.
type sitelist struct {
	sites    map[string]*site
	siteLock sync.RWMutex

	errNoSuchHost *resource
	errNoSuchFile *resource

	defErrNoSuchHost *resource
	defErrNoSuchFile *resource

	root        string
	devmode     uint32
	defaulthost string
}

func (sl *sitelist) status() string {
	sl.siteLock.RLock()
	defer sl.siteLock.RUnlock()

	var buf string

	buf += fmt.Sprintf("Sites (%d):\n", len(sl.sites))
	for host, site := range sl.sites {
		site.RLock()
		buf += fmt.Sprintf("\t%s (%d HTTP resources, %d HTTPS resources)\n", host, len(site.http), len(site.https))
		site.RUnlock()
	}

	buf += fmt.Sprintf("Root: %s\n", sl.root)
	buf += fmt.Sprintf("Dev mode: %t\n", atomic.LoadUint32(&sl.devmode) == 1)
	buf += fmt.Sprintf("No such host: %t\n", sl.errNoSuchHost == nil)
	buf += fmt.Sprintf("No such file: %t\n", sl.errNoSuchFile == nil)

	return buf
}

// dev flips the development mode switch.
func (sl *sitelist) dev(active bool) {
	if active {
		atomic.StoreUint32(&sl.devmode, 1)
	} else {
		atomic.StoreUint32(&sl.devmode, 0)
	}
}

// fetch retrieves the file for a given URL. This is where the work happens, so
// it must stay simple and fast.
func (sl *sitelist) fetch(url *url.URL) (*resource, int) {
	var (
		host, p, fancypath string
		err                error
		exists             bool
		s                  *site
		res                *resource
		body               []byte
		rmap               map[string]*resource
	)

	if host, _, err = net.SplitHostPort(url.Host); err != nil {
		host = url.Host
	}

	// If the devmode flag is set, we reload the entire sitelist. This is by far
	// the easiest.
	if atomic.LoadUint32(&sl.devmode) == 1 {
		sl.load()
	}

	sl.siteLock.RLock()
	s, exists = sl.sites[host]
	sl.siteLock.RUnlock()

	// Check if the host existed. If not, serve a 500 no such service.
	if !exists {
		if sl.errNoSuchHost != nil {
			return sl.errNoSuchHost, 500
		}
		return sl.defErrNoSuchHost, 500
	}

	p = path.Clean(url.Path)

	// Check if we need to serve directly from disk. We verify if the fancypath
	// path prefix matches, and if so, try to load a resource directly, without
	// storing it in the resource map. The resource is loaded from the "fancy"
	// folder of the vhost directory. If the file cannot be read for any reason,
	// we will skip to the 404 handling.
	fancypath = s.config.General.FancyFolder
	if fancypath != "" && len(p) > len(fancypath) && p[:len(fancypath)] == fancypath {
		p = path.Join(sl.root, host, "fancy", p)
		if body, err = ioutil.ReadFile(p); err != nil {
			log.Printf("Error trying to read file \"%s\" from disk: %v", p, err)
			goto filenotfound
		}
		res = &resource{
			path:     p,
			body:     body,
			config:   s.config,
			fromDisk: true,
		}
		res.update()
		return res, 200
	}

	// Select the site based on the schema.
	switch url.Scheme {
	case "https":
		rmap = s.https
	default:
		rmap = s.http
	}

	s.RLock()
	res, exists = rmap[p]
	s.RUnlock()

	if exists {
		return res, 200
	}

filenotfound:
	// 404 File Not Found time! We try to fetch a 404.html document from the
	// site. The 404 document is served from 3 locations, as available:
	//
	// * The vhost directory itself, if available.
	// * The root directory itself, if available.
	// * The configured default document.
	//
	// Note that all but the configured 404 document can be requested directly,
	// in which case they will return a 200 OK, not a 404 File Not Found.
	s.RLock()
	res, exists = rmap["/404.html"]
	s.RUnlock()
	if exists {
		return res, 404
	}

	if sl.errNoSuchFile != nil {
		return sl.errNoSuchFile, 404
	}

	return sl.defErrNoSuchFile, 404
}

// load reads a root server structure in the format:
//
//		example.com/scheme/index.html
//      example.com/config.toml
//
// The initial part of the path is the domain name, used for vhost purposes. The
// second part is the scheme, which can be "http", "https", "common" or "fancy".
// The first two are unique to their scheme. Files in "common" are available
// from both HTTP and HTTPS. Files in "fancy" are used to be served directly
// from disk, and is loaded on-request. The configuration specifies which
// (virtual) folder should serve files from fancy. All other files are served
// completely from memory.
func (sl *sitelist) load() error {
	log.Printf("Reloading root")
	// We store the results of the load in local temporary variables, and only
	// install them on success.
	var (
		sites         = make(map[string]*site)
		errNoSuchHost *resource
		errNoSuchFile *resource
	)

	// list root
	files, err := ioutil.ReadDir(sl.root)
	if err != nil {
		return err
	}

	for _, s := range files {
		name := s.Name()

		p := path.Join(sl.root, name)

		if !s.IsDir() {
			// Check if it's any server-global resources.
			switch name {
			case "404.html":
				body, err := ioutil.ReadFile(p)
				if err != nil {
					return err
				}
				errNoSuchFile = &resource{
					path:   p,
					body:   body,
					config: &DefaultSiteConfig,
				}
				errNoSuchFile.update()
			case "500.html":
				body, err := ioutil.ReadFile(p)
				if err != nil {
					return err
				}
				errNoSuchHost = &resource{
					path:   p,
					body:   body,
					config: &DefaultSiteConfig,
				}
				errNoSuchHost.update()
			}

			continue
		}

		schemes, err := ioutil.ReadDir(p)
		if err != nil {
			return err
		}

		conf, err := readSiteConf(path.Join(sl.root, name, "config.toml"))
		if err != nil {
			if conf == nil {
				return err
			}
			log.Printf("Cannot read configuration for %s, using default: %v", name, err)
		}

		s := newSite(conf)

		sites[name] = s

		for _, c := range schemes {
			if !c.IsDir() {
				continue
			}

			scheme := c.Name()
			start := path.Join(sl.root, name, scheme)
			err := filepath.Walk(start, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}

				p2 := p[len(start):]
				if len(p2) == 0 {
					p2 = "/"
				}

				http := scheme == "http" || scheme == "common"
				https := scheme == "https" || scheme == "common"
				return s.addResource(p, p2, http, https)

			})

			if err != nil {
				return err
			}
		}
	}

	// We're done, so install the results.
	sl.siteLock.Lock()
	sl.sites = sites
	sl.errNoSuchFile = errNoSuchFile
	sl.errNoSuchHost = errNoSuchHost
	sl.siteLock.Unlock()

	return nil
}

// cmdhttp is the command handler. It implements http.Handler.
func (sl *sitelist) cmdhttp(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/devel":
		log.Printf("[%s]: enabling development mode", req.RemoteAddr)
		sl.dev(true)

		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("OK\n"))
	case "/prod":
		log.Printf("[%s]: disabling development mode", req.RemoteAddr)
		sl.dev(false)
		log.Printf("[%s]: reloading", req.RemoteAddr)
		if err := sl.load(); err != nil {
			log.Printf("[%s]: reload failed: %v", req.RemoteAddr, err)
			w.WriteHeader(500)
			w.Write([]byte(fmt.Sprintf("reload failed: %v\n", err)))
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("OK\n"))
	case "/reload":
		log.Printf("[%s]: reloading", req.RemoteAddr)
		if err := sl.load(); err != nil {
			log.Printf("[%s]: reload failed: %v", req.RemoteAddr, err)
			w.WriteHeader(500)
			w.Write([]byte(fmt.Sprintf("reload failed: %v\n", err)))
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("OK\n"))
	case "/status":
		log.Printf("[%s]: status request", req.RemoteAddr)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(sl.status()))
	default:
		w.WriteHeader(404)
		w.Write([]byte("Unknown command\n"))
	}
}

// quickHeaderGet bypasses net/textproto/MIMEHeader.CanonicalMIMEHeaderKey,
// which would otherwise have been run on the key. This is a waste of time if we
// manually canonicalize the query key.
func quickHeaderGet(key string, h http.Header) (string, bool) {
	val := h[key]
	if len(val) == 0 {
		return "", false
	}
	return val[0], true
}

// quickHeaderGetLast works like quickHeaderGet, but fetches the last instance
// of the header.
func quickHeaderGetLast(key string, h http.Header) (string, bool) {
	val := h[key]
	if len(val) == 0 {
		return "", false
	}
	return val[len(val)-1], true
}

// http is the actual HTTP handler, serving the requests as quickly as it can.
// It implements http.Handler.
func (sl *sitelist) http(w http.ResponseWriter, req *http.Request) {
	var (
		ifNoneMatch, ifModifiedSince, acceptEncoding string
		head, cacheResponse, exists                  bool
		now                                          = time.Now()
		h                                            = w.Header()
		r                                            *resource
		body                                         []byte
		status                                       int
		imsdate                                      time.Time
		err                                          error
	)

	// We patch up the URL object for convenience.
	if req.Host == "" {
		req.URL.Host = sl.defaulthost
	} else {
		req.URL.Host = req.Host
	}
	if req.TLS != nil {
		req.URL.Scheme = "https"
	} else {
		req.URL.Scheme = "http"
	}

	// Evaluate method
	switch req.Method {
	case "GET":
		// do nothing
	case "HEAD":
		head = true
	default:
		h["Content-Type"] = []string{"text/plain"}
		w.WriteHeader(405)
		w.Write([]byte("method not allowed"))
		access(req, 405)
		return
	}

	r, status = sl.fetch(req.URL)

	// Set headers
	if r.cnttype != "" {
		h["Content-Type"] = []string{r.cnttype}
	}
	h["Etag"] = []string{r.hash}
	h["Date"] = []string{now.Format(time.RFC1123)}
	h["Last-Modified"] = []string{r.loaded.Format(time.RFC1123)}
	h["Cache-Control"] = []string{fmt.Sprintf("public, max-age=%.0f", r.cache.Seconds())}
	h["Expires"] = []string{now.Add(r.cache).Format(time.RFC1123)}
	h["Vary"] = []string{"Accept-Encoding"}

	// Should we send a 304 Not Modified? We do this if the file was found, and
	// the cache information headers from the client match the file we have
	// available. 304's are, annoyingly so, only partially like HEAD request, as
	// it does not contain encoding and length information about the payload
	// like a HEAD does. This means we have to interrupt the request at
	// different points of their execution... *sigh*
	if status == 200 {
		cacheResponse = false
		if ifNoneMatch, exists = quickHeaderGet("If-None-Match", req.Header); exists {
			cacheResponse = ifNoneMatch == r.hash
		} else if ifModifiedSince, exists = quickHeaderGet("If-Modified-Since", req.Header); exists {
			imsdate, err = time.Parse(time.RFC1123, ifModifiedSince)
			cacheResponse = err == nil && imsdate.After(r.loaded)
		}

		if cacheResponse {
			w.WriteHeader(304)
			access(req, 304)
			return
		}
	}

	body = r.body

	// Compressed?
	if r.gzip != nil {
		if acceptEncoding, exists = quickHeaderGet("Accept-Encoding", req.Header); exists && strings.Contains(acceptEncoding, "gzip") {
			h["Content-Encoding"] = []string{"gzip"}
			body = r.gzip
		}
	}
	h["Content-Length"] = []string{fmt.Sprintf("%d", len(body))}

	w.WriteHeader(status)
	access(req, status)

	// HEAD?
	if head {
		return
	}

	if _, err = w.Write(body); err != nil {
		log.Printf("[%s]: error writing response: %v", req.RemoteAddr, err)
	}
}
