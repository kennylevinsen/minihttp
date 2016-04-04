package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
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

var (
	defaultFile = flag.String("defaultFile", "index.html", "the file to serve when a dir is requested")
	rootdir     = flag.String("rootdir", "", "the dir to serve from")
	fromdisk    = flag.String("fromdisk", "", "a dir relative to rootdir to always serve fresh from disk without cache")
	development = flag.Bool("dev", false, "reload on every request")
	address     = flag.String("address", "", "address to listen on")
	tls         = flag.String("tls", "", "address to serve TLS on")
	tlscert     = flag.String("tlscert", "", "tls certificate path")
	tlskey      = flag.String("tlskey", "", "tls key path")
	logfile     = flag.String("logfile", "", "file to write log to")
	cmdaddr     = flag.String("cmdaddr", "", "addr to listen for cmds on")
	defaulthost = flag.String("defaulthost", "", "default host when none is provided")
)

type resource struct {
	body    []byte
	gzip    []byte
	cnttype string
	cache   time.Duration
	loaded  time.Time
	hash    string
	path    string
}

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

	ext := path.Ext(p)

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
	gz.Write(body)
	gz.Close()
	r.gzip = buf.Bytes()

	if float64(len(r.gzip))*1.1 >= float64(len(body)) {
		// The compression is too weak.
		r.gzip = nil
	}

	r.body = body
	h := sha256.Sum256(body)
	r.hash = hex.EncodeToString(h[:])
	r.cnttype = mime.TypeByExtension(ext)
	r.loaded = time.Now()
	return nil
}

var (
	defErrNoSuchHost = &resource{
		body:    []byte("no such service"),
		cnttype: "text/plain",
	}
	defErrNoSuchFile = &resource{
		body:    []byte("no such file"),
		cnttype: "text/plain",
	}
)

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

	root      string
	fancypath string
	devmode   uint32
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

	var smap map[string]*site
	switch url.Scheme {
	case "https":
		smap = sl.httpsSites
	default:
		smap = sl.httpSites
	}

	if atomic.LoadUint32(&sl.devmode) == 1 {
		sl.load()
	}

	sl.siteLock.RLock()
	s, exists := smap[host]
	sl.siteLock.RUnlock()
	if !exists {
		if sl.errNoSuchHost != nil {
			return sl.errNoSuchHost, 500
		}
		return defErrNoSuchHost, 500
	}

	var res *resource

	p := path.Clean(url.Path)
	if sl.fancypath != "" && len(p) > len(sl.fancypath) && p[:len(sl.fancypath)] == sl.fancypath {
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
	p = path.Join("/404.html")
	s.RLock()
	res, exists = s.resources[p]
	s.RUnlock()
	if exists {
		return res, 404
	}

	if sl.errNoSuchFile != nil {
		return sl.errNoSuchFile, 404
	}

	return defErrNoSuchFile, 404
}

func (sl *sitelist) load() error {
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

		components, err := ioutil.ReadDir(path.Join(sl.root, name))
		if err != nil {
			return err
		}

		for _, c := range components {
			if !c.IsDir() {
				continue
			}

			component := c.Name()
			start := path.Join(sl.root, name, component)
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

				if component == "http" || component == "common" {
					if _, exists := httpSites[name]; !exists {
						httpSites[name] = &site{
							resources: make(map[string]*resource),
						}
					}
					httpSites[name].resources[p2] = res
				}

				if component == "https" || component == "common" {
					if _, exists := httpsSites[name]; !exists {
						httpsSites[name] = &site{
							resources: make(map[string]*resource),
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

	sl.siteLock.Lock()
	sl.httpSites = httpSites
	sl.httpsSites = httpsSites
	sl.errNoSuchFile = errNoSuchFile
	sl.errNoSuchHost = errNoSuchHost
	sl.siteLock.Unlock()

	return nil
}

func access(req *http.Request, status int) {
	log.Printf("[%s]: %s \"%v\" %s %d", req.RemoteAddr, req.Method, req.URL, req.Proto, status)
}

func main() {
	var err error
	flag.Parse()
	if *logfile != "" {
		file, err := os.OpenFile(*logfile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			fmt.Printf("Unable to open logfile: %s", err)
			return
		}
		log.SetOutput(file)
	}

	if *rootdir == "" {
		log.Printf("Missing directory to serve")
		flag.Usage()
		return
	}

	if *address == "" && *tls == "" {
		log.Printf("Missing address to serve")
		flag.Usage()
		return
	}

	if *tls != "" && (*tlscert == "" || *tlskey == "") {
		log.Printf("Missing key/cert for tls")
		flag.Usage()
		return
	}

	sl := &sitelist{root: *rootdir}

	if err = sl.load(); err != nil {
		log.Printf("Unable to walk files: %v", err)
		return
	}

	sl.dev(*development)

	if *cmdaddr != "" {
		go func() {
			cmdhandler := func(w http.ResponseWriter, req *http.Request) {
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
					if err = sl.load(); err != nil {
						log.Printf("[%s]: reload failed: %v", req.RemoteAddr, err)
						w.WriteHeader(500)
						w.Write([]byte(fmt.Sprintf("reload failed: %v\n", err)))
						return
					}

					w.Header().Set("Content-Type", "text/plain")
					w.Write([]byte("OK\n"))
				case "/reload":
					log.Printf("[%s]: reloading", req.RemoteAddr)
					if err = sl.load(); err != nil {
						log.Printf("[%s]: reload failed: %v", req.RemoteAddr, err)
						w.WriteHeader(500)
						w.Write([]byte(fmt.Sprintf("reload failed: %v\n", err)))
						return
					}

					w.Header().Set("Content-Type", "text/plain")
					w.Write([]byte("OK\n"))
				default:
					w.WriteHeader(404)
					w.Write([]byte("Unknown command\n"))
				}
			}

			s := &http.Server{
				Addr:         *cmdaddr,
				Handler:      http.HandlerFunc(cmdhandler),
				ReadTimeout:  10 * time.Second,
				WriteTimeout: 10 * time.Second,
			}
			log.Fatal(s.ListenAndServe())
		}()
	}

	handler := func(w http.ResponseWriter, req *http.Request) {
		// We patch up the URL object for convenience.
		if req.Host == "" {
			req.URL.Host = *defaulthost
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

		// Set headers
		h := w.Header()
		if r.cnttype != "" {
			h.Set("Content-Type", r.cnttype)
		}

		now := time.Now()
		if r.hash != "" {
			h.Set("Etag", r.hash)
		}
		h.Set("Date", now.Format(time.RFC1123))
		h.Set("Last-Modified", r.loaded.Format(time.RFC1123))
		h.Set("Vary", "Accept-Encoding")

		// Cacheable?
		if r.cache == 0 {
			h.Set("Cache-Control", fmt.Sprintf("public, max-age=0, no-cache"))
			h.Set("Expires", now.Format(time.RFC1123))
		} else {
			h.Set("Cache-Control", fmt.Sprintf("public, max-age=%.0f", r.cache.Seconds()))
			h.Set("Expires", now.Add(r.cache).Format(time.RFC1123))
		}

		// Should we send a 304 Not Modified?
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

	var wg sync.WaitGroup
	if *address != "" {
		wg.Add(1)
		go func() {
			s := &http.Server{
				Addr:         *address,
				Handler:      http.HandlerFunc(handler),
				ReadTimeout:  30 * time.Second,
				WriteTimeout: 10 * time.Minute,
			}
			log.Fatal(s.ListenAndServe())
			wg.Done()
		}()
	}

	if *tls != "" {
		wg.Add(1)
		go func() {
			s := &http.Server{
				Addr:         *tls,
				Handler:      http.HandlerFunc(handler),
				ReadTimeout:  30 * time.Second,
				WriteTimeout: 10 * time.Minute,
			}

			log.Fatal(s.ListenAndServeTLS(*tlscert, *tlskey))
			wg.Done()
		}()
	}

	wg.Wait()

}
