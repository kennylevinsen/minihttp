package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	log.Printf("[%s]: %s \"%v\" %s %d", req.RemoteAddr, req.Method, req.URL, req.Proto, status)
}

type resource struct {
	body    []byte
	gzip    []byte
	cnttype string
	cache   time.Duration
	loaded  time.Time
	hash    string
	path    string
}

// update updates all information about the resource, given a body and a path.
// Its use eases synthetic content creation.
func (r *resource) update() error {
	ext := path.Ext(r.path)

	switch ext {
	case ".woff", ".woff2", ".ttf", ".eot", ".otf", ".jpg", ".png":
		r.cache = 90 * 24 * time.Hour
	case ".css":
		r.cache = 7 * 24 * time.Hour
	default:
		r.cache = time.Hour
	}

	buf := new(bytes.Buffer)
	gz := gzip.NewWriter(buf)
	gz.Write(r.body)
	gz.Close()
	r.gzip = buf.Bytes()

	if float64(len(r.gzip))*1.1 >= float64(len(r.body)) {
		// The compression is too weak.
		r.gzip = nil
	}

	h := sha256.Sum256(r.body)
	r.hash = hex.EncodeToString(h[:])
	r.cnttype = mime.TypeByExtension(ext)
	r.loaded = time.Now()

	return nil
}

// load reads a file from the path, and calls update.
func (r *resource) load() error {
	p := r.path
	s, err := os.Stat(p)
	if err != nil {
		return nil
	}

	if s.IsDir() {
		p = path.Join(p, *defaultFile)
		if s, err := os.Stat(p); err != nil || s.IsDir() {
			return nil
		}
	}

	body, err := ioutil.ReadFile(p)
	if err != nil {
		return err
	}

	r.body = body
	return r.update()
}

type site struct {
	sync.RWMutex
	resources map[string]*resource
	fancypath string
}

type sitelist struct {
	httpSites  map[string]*site
	httpsSites map[string]*site
	siteLock   sync.RWMutex

	errNoSuchHost *resource
	errNoSuchFile *resource

	defErrNoSuchHost *resource
	defErrNoSuchFile *resource

	root        string
	fancypath   string
	devmode     uint32
	defaulthost string
}

func (sl *sitelist) status() string {
	sl.siteLock.RLock()
	defer sl.siteLock.RUnlock()

	var buf string

	buf += fmt.Sprintf("HTTP sites (%d):\n", len(sl.httpSites))
	for host, site := range sl.httpSites {
		site.RLock()
		buf += fmt.Sprintf("\t%s (%d resources)\n", host, len(site.resources))
		site.RUnlock()
	}

	buf += fmt.Sprintf("HTTPS sites (%d):\n", len(sl.httpsSites))
	for host, site := range sl.httpsSites {
		site.RLock()
		buf += fmt.Sprintf("\t%s (%d resources)\n", host, len(site.resources))
		site.RUnlock()
	}

	buf += fmt.Sprintf("Root: %s\n", sl.root)
	buf += fmt.Sprintf("Fancy path: %s\n", sl.fancypath)
	buf += fmt.Sprintf("Dev mode: %t\n", atomic.LoadUint32(&sl.devmode) == 1)
	buf += fmt.Sprintf("No such host: %t\n", sl.errNoSuchHost == nil)
	buf += fmt.Sprintf("No such file: %t\n", sl.errNoSuchFile == nil)

	return buf
}

func (sl *sitelist) dev(active bool) {
	if active {
		atomic.StoreUint32(&sl.devmode, 1)
	} else {
		atomic.StoreUint32(&sl.devmode, 0)
	}
}

func (sl *sitelist) fetch(url *url.URL) (*resource, int) {
	host, _, err := net.SplitHostPort(url.Host)
	if err != nil {
		host = url.Host
	}

	// Select the site based on the schema.
	var smap map[string]*site
	switch url.Scheme {
	case "https":
		smap = sl.httpsSites
	default:
		smap = sl.httpSites
	}

	// If the devmode flag is set, we reload the entire sitelist. This is by far
	// the easiest.
	if atomic.LoadUint32(&sl.devmode) == 1 {
		sl.load()
	}

	sl.siteLock.RLock()
	s, exists := smap[host]
	sl.siteLock.RUnlock()

	// Check if the host existed. If not, serve a 500 no such service.
	if !exists {
		if sl.errNoSuchHost != nil {
			return sl.errNoSuchHost, 500
		}
		return sl.defErrNoSuchHost, 500
	}

	var res *resource
	p := path.Clean(url.Path)

	// Check if we need to serve directly from disk. We verify if the fancypath
	// path prefix matches, and if so, try to load a resource directly, without
	// storing it in the resource map. The resource is loaded from the "fancy"
	// folder of the vhost directory. If the file cannot be read for any reason,
	// we will skip to the 404 handling.
	if s.fancypath != "" && len(p) > len(s.fancypath) && p[:len(s.fancypath)] == s.fancypath {
		p = path.Join(sl.root, host, "fancy", p)
		if s, err := os.Stat(p); err != nil || s.IsDir() {
			goto filenotfound
		}
		res = &resource{path: p}
		if err = res.load(); err != nil {
			goto filenotfound
		}
		res.cache = 0

		return res, 200
	}

	s.RLock()
	res, exists = s.resources[p]
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
	res, exists = s.resources["/404.html"]
	s.RUnlock()
	if exists {
		return res, 404
	}

	if sl.errNoSuchFile != nil {
		return sl.errNoSuchFile, 404
	}

	return sl.defErrNoSuchFile, 404
}

func (sl *sitelist) load() error {
	// load reads a root server structure in the format:
	//
	//		example.com/http/index.html
	//
	// The initial part of the path is the domain name, used for vhost purposes.
	// The second part is the scheme, which can be "http", "https", "common" or
	// "fancy". The first two are unique to their scheme. Files in "common" are
	// available from both HTTP and HTTPS. Files in "fancy" are used to be
	// served directly from disk, and is loaded on-request. All other files are
	// served completely from memory.

	// We store the results of the load in local temporary variables, and only
	// install them on success.
	var (
		httpSites     = make(map[string]*site)
		httpsSites    = make(map[string]*site)
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

		if !s.IsDir() {
			// Check if it's any server-global resources.
			switch name {
			case "404.html":
				errNoSuchFile = &resource{path: path.Join(sl.root, name)}
				if err := errNoSuchFile.load(); err != nil {
					return err
				}
			case "500.html":
				errNoSuchHost = &resource{path: path.Join(sl.root, name)}
				if err := errNoSuchHost.load(); err != nil {
					return err
				}
			}

			continue
		}

		schemes, err := ioutil.ReadDir(path.Join(sl.root, name))
		if err != nil {
			return err
		}

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

				res := &resource{path: p}
				err = res.load()
				if err != nil {
					return err
				}

				if scheme == "http" || scheme == "common" {
					if _, exists := httpSites[name]; !exists {
						httpSites[name] = &site{
							resources: make(map[string]*resource),
							fancypath: sl.fancypath,
						}
					}
					httpSites[name].resources[p2] = res
				}

				if scheme == "https" || scheme == "common" {
					if _, exists := httpsSites[name]; !exists {
						httpsSites[name] = &site{
							resources: make(map[string]*resource),
							fancypath: sl.fancypath,
						}
					}
					httpsSites[name].resources[p2] = res
				}

				return nil
			})

			if err != nil {
				return err
			}
		}
	}

	// We're done, so install the results.
	sl.siteLock.Lock()
	sl.httpSites = httpSites
	sl.httpsSites = httpsSites
	sl.errNoSuchFile = errNoSuchFile
	sl.errNoSuchHost = errNoSuchHost
	sl.siteLock.Unlock()

	return nil
}

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

func (sl *sitelist) http(w http.ResponseWriter, req *http.Request) {
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
	var head bool
	switch req.Method {
	case "GET":
		// do nothing
	case "HEAD":
		head = true
	default:
		w.WriteHeader(405)
		w.Write([]byte("<!doctype html><html><body><h1>405 Method Not Allowed</h1></body></html>\n"))
		access(req, 405)
		return
	}

	r, status := sl.fetch(req.URL)
	now := time.Now()

	// Set headers
	h := w.Header()
	if r.cnttype != "" {
		h.Set("Content-Type", r.cnttype)
	}
	h.Set("Etag", r.hash)
	h.Set("Date", now.Format(time.RFC1123))
	h.Set("Last-Modified", r.loaded.Format(time.RFC1123))
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%.0f", r.cache.Seconds()))
	h.Set("Expires", now.Add(r.cache).Format(time.RFC1123))
	h.Set("Vary", "Accept-Encoding")

	// Should we send a 304 Not Modified? We do this if the file was found, and
	// the cache information headers from the client match the file we have
	// available. 304's are, annoyingly so, only partially like HEAD request, as
	// it does not contain encoding and length information about the payload
	// like a HEAD does. This means we have to interrupt the request at
	// different points of their execution... *sigh*
	if status == 200 {
		cacheResponse := false
		ifModifiedSince := req.Header.Get("If-Modified-Since")
		ifNoneMatch := req.Header.Get("If-None-Match")
		if ifNoneMatch != "" {
			cacheResponse = ifNoneMatch == r.hash
		} else if ifModifiedSince != "" {
			imsdate, err := time.Parse(time.RFC1123, ifModifiedSince)
			cacheResponse = err == nil && imsdate.After(r.loaded)
		}

		if cacheResponse {
			w.WriteHeader(304)
			access(req, 304)
			return
		}
	}

	// Compressed?
	var b []byte
	if r.gzip != nil && strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		b = r.gzip
	} else {
		b = r.body
	}
	h.Set("Content-Length", fmt.Sprintf("%d", len(b)))

	w.WriteHeader(status)
	access(req, status)

	// HEAD?
	if head {
		return
	}

	if _, err := w.Write(b); err != nil {
		log.Printf("[%s]: error writing response: %v", req.RemoteAddr, err)
	}
}
