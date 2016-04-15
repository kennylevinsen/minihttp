package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	defaultNoSuchHost = &resource{
		body:    []byte("no such host"),
		loaded:  time.Now(),
		cnttype: "text/plain; charset=utf-8",
		cache:   "public, max-age=0, no-cache",
		hash:    "W/\"go-far-away\"",
		path:    "/403.html",
	}

	defaultNoSuchFile = &resource{
		body:    []byte("no such file"),
		loaded:  time.Now(),
		cnttype: "text/plain; charset=utf-8",
		cache:   "public, max-age=0, no-cache",
		hash:    "W/\"go-away\"",
		path:    "/404.html",
	}
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

// sitelist manages a set of sites.
type sitelist struct {
	sites    map[string]*site
	siteLock sync.RWMutex

	errNoSuchHost *resource
	errNoSuchFile *resource

	root        string
	devmode     uint32
	defaulthost string

	// stats
	filesInMemory      int
	plainBytesInMemory int
	gzipBytesInMemory  int
}

func (sl *sitelist) status() string {
	sl.siteLock.RLock()
	defer sl.siteLock.RUnlock()

	var sites string

	for host, site := range sl.sites {
		sites += fmt.Sprintf("\t%s (%d HTTP resources, %d HTTPS resources)\n", host, len(site.http), len(site.https))
	}

	return fmt.Sprintf(`
Sites: (%d):
	%s

Settings:
	Root:                %s
	Dev mode:            %t
	Global no such host: %t
	Global no such file: %t

Stats:
	Total plain file size: %dB
	Total gzip file size:  %dB
	Total files:           %d
	`,
		len(sl.sites),
		sites,
		sl.root,
		atomic.LoadUint32(&sl.devmode) == 1,
		sl.errNoSuchHost == nil,
		sl.errNoSuchFile == nil,
		sl.plainBytesInMemory,
		sl.gzipBytesInMemory,
		sl.filesInMemory)
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

	// Check if the host existed. If not, check the default host, and if that's not there either, return a 403
	if !exists {
		sl.siteLock.RLock()
		s, exists = sl.sites[sl.defaulthost]
		sl.siteLock.RUnlock()
		if !exists {
			if sl.errNoSuchHost != nil {
				return sl.errNoSuchHost, http.StatusForbidden
			}
			return defaultNoSuchHost, http.StatusForbidden
		}
	}

	// TODO(kl): Consider moving this to the from-disk branch. That means that
	// /meticulous/../fantastical will not render correctly as /fantastical, but
	// I do not really find this to be an issue. It could shave some cycles off
	// in-memory resource fetch.
	p = path.Clean(url.Path)

	// First, let's try for the file in memory. If it's found, we return it
	// immediately. This is the path we want to be the fastest.
	switch url.Scheme {
	case "https":
		rmap = s.https
	default:
		rmap = s.http
	}

	if res, exists = rmap[p]; exists {
		return res, 200
	}

	// The file was not in memory, so see if it's available in the from-disk
	// folder. We first verify if the path prefix matches the permitted
	// from-disk prefix, and if so, try to load the resource directly, without
	// storing it in the resource map. The source is loaded from the "fancy"
	// folder of the vhost directory.
	fancypath = s.config.General.FancyFolder
	if strings.HasPrefix(p, fancypath) {
		p = path.Join(sl.root, host, "fancy", p)

		// We make a streaming resource. The beefit of this is a much lower
		// time-to-first-byte, as well as lower memory consumption.
		if x, err := os.Open(p); err == nil {
			if fi, err := x.Stat(); err == nil && !fi.IsDir() {
				res = &resource{
					bodyReadCloser: x,
					path:           p,
					config:         s.config,
					loaded:         fi.ModTime(),
					hash:           fmt.Sprintf("W/\"%x-%xi\"", fi.ModTime().Unix(), fi.Size()),
					ghash:          fmt.Sprintf("W/\"%x-%xg\"", fi.ModTime().Unix(), fi.Size()),
					fromDisk:       true,
					permitGZIP:     true,
				}
				res.update()
				return res, 200
			}

			// We couldn't use the file, so close it up and continue to file not
			// found handling.
			x.Close()
		}
	}

	// 404 File Not Found time! We try to fetch a 404.html document from the
	// site. The 404 document is served from 3 locations, as available:
	//
	// * The vhost directory itself, if available.
	// * The root directory itself, if available.
	// * The configured default document.
	//
	if res, exists = rmap["/404.html"]; exists {
		return res, http.StatusNotFound
	}

	if sl.errNoSuchFile != nil {
		return sl.errNoSuchFile, http.StatusNotFound
	}

	return defaultNoSuchFile, http.StatusNotFound
}

// http is the actual HTTP handler, serving the requests as quickly as it can.
// It implements http.Handler.
func (sl *sitelist) http(w http.ResponseWriter, req *http.Request) {
	var (
		head, exists, useGZIP bool
		now                   = time.Now()
		h                     = w.Header()
		err                   error
	)

	// We patch up the URL object for convenience.
	req.URL.Host = req.Host
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
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("method not allowed"))
		access(req, http.StatusMethodNotAllowed)
		return
	}

	r, status := sl.fetch(req.URL)

	hash := r.hash
	body := r.body
	if r.permitGZIP {
		var acceptEncoding string
		if acceptEncoding, exists = quickHeaderGet("Accept-Encoding", req.Header); exists && strings.Contains(acceptEncoding, "gzip") {
			useGZIP = true
			body = r.gzip
			hash = r.ghash
			h["Content-Encoding"] = []string{"gzip"}
		}
	}

	// Set headers
	if r.cnttype != "" {
		h["Content-Type"] = []string{r.cnttype}
	}
	h["Date"] = []string{now.Format(time.RFC1123)}
	h["Last-Modified"] = []string{r.loaded.Format(time.RFC1123)}
	h["Cache-Control"] = []string{r.cache}
	h["Vary"] = []string{"Accept-Encoding"}
	h["Etag"] = []string{hash}

	// Should we send a 304 Not Modified? We do this if the file was found, and
	// the cache information headers from the client match the file we have
	// available. 304's are, annoyingly so, only partially like HEAD request, as
	// it does not contain encoding and length information about the payload
	// like a HEAD does. This means we have to interrupt the request at
	// different points of their execution... *sigh*
	if status == http.StatusOK {
		var cacheResponse bool
		var ifNoneMatch, ifModifiedSince string
		if ifNoneMatch, exists = quickHeaderGet("If-None-Match", req.Header); exists {
			cacheResponse = ifNoneMatch == r.hash || ifNoneMatch == "*"
		} else if ifModifiedSince, exists = quickHeaderGet("If-Modified-Since", req.Header); exists {
			var imsdate time.Time
			imsdate, err = time.Parse(time.RFC1123, ifModifiedSince)
			cacheResponse = err == nil && !imsdate.IsZero() && imsdate.After(r.loaded)
		}

		if cacheResponse {
			w.WriteHeader(http.StatusNotModified)
			access(req, http.StatusNotModified)
			return
		}
	}

	// Are we dealing with a streaming resource (That is, a file)?
	if r.bodyReadCloser != nil {
		w.WriteHeader(status)
		access(req, status)
		if head {
			return
		}

		var ww io.Writer = w
		if useGZIP {
			gz, _ := gzip.NewWriterLevel(w, 6)
			defer gz.Close()
			ww = gz
		}

		if _, err = io.Copy(ww, r.bodyReadCloser); err != nil {
			log.Printf("[%s]: error writing response: %v", req.RemoteAddr, err)
		}
		r.bodyReadCloser.Close()
		return
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

// load reads a root server structure in the format:
//
//      example.com/scheme/index.html
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
	// install them on success. It also shortens the time we need to hold the
	// lock for reload.
	var (
		sites         = make(map[string]*site)
		errNoSuchHost *resource
		errNoSuchFile *resource
		n             = time.Now()
	)

	// list root
	files, err := ioutil.ReadDir(sl.root)
	if err != nil {
		return err
	}

	cachemap := make(map[string]*cache)

	for _, s := range files {
		name := s.Name()
		p := path.Join(sl.root, name)

		// If we found a directory, check if it's a server global resource.
		if !s.IsDir() {
			res := &resource{
				path:   p,
				config: &DefaultSiteConfig,
				loaded: n,
			}

			switch name {
			case "404.html":
				errNoSuchFile = res
			case "403.html":
				errNoSuchHost = res
			default:
				continue
			}

			if res.body, err = ioutil.ReadFile(p); err != nil {
				return err
			}
			res.updateTagCompress()

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
			http := scheme == "http" || scheme == "common"
			https := scheme == "https" || scheme == "common"
			start := path.Join(sl.root, name, scheme)
			err := filepath.Walk(start, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}

				p2 := p[len(start):]
				if len(p2) == 0 {
					p2 = "/"
				}

				return s.addResource(p, p2, cachemap, http, https)
			})

			if err != nil {
				return err
			}
		}
	}

	var plainInMemory, gzipInMemory int
	for _, v := range cachemap {
		plainInMemory += len(v.plain)
		gzipInMemory += len(v.gzip)
	}

	// We're done, so install the results.
	sl.siteLock.Lock()
	sl.sites = sites
	sl.errNoSuchFile = errNoSuchFile
	sl.errNoSuchHost = errNoSuchHost
	sl.filesInMemory = len(cachemap)
	sl.plainBytesInMemory = plainInMemory
	sl.gzipBytesInMemory = gzipInMemory
	sl.siteLock.Unlock()

	// We might have created a lot of garbage, so just run the GC now.
	runtime.GC()

	return nil
}
