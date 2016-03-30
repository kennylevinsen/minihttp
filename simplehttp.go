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
	"net/http"
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
)

var (
	indevmode    uint32
	resources    map[string]*resource
	resourceLock sync.RWMutex
)

type resource struct {
	body    []byte
	gzip    []byte
	cnttype string
	cache   time.Duration
	loaded  time.Time
	hash    string
}

func (r *resource) load(p string) error {
	s, err := os.Stat(p)
	if err != nil {
		return nil
	}

	if s.IsDir() {
		p = path.Join(p, *defaultFile)
		s, err := os.Stat(p)
		if err != nil {
			return nil
		}
		if s.IsDir() {
			return nil
		}
	}

	body, err := ioutil.ReadFile(p)
	if err != nil {
		return err
	}

	ext := path.Ext(p)

	switch ext {
	case ".woff", ".woff2", ".ttf", ".eot", ".otf":
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

func reload(p string) (map[string]*resource, error) {
	res := make(map[string]*resource)
	err := filepath.Walk(p, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		r := &resource{}
		if err = r.load(p); err != nil {
			return err
		}

		res[p] = r

		return nil
	})

	return res, err
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

	resources, err = reload(*rootdir)

	if err != nil {
		log.Printf("Unable to walk files: %v", err)
		return
	}

	if *development {
		indevmode = 1
	}

	if *cmdaddr != "" {
		go func() {
			cmdhandler := func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/devel":
					log.Printf("[%s]: enabling development mode", req.RemoteAddr)
					atomic.StoreUint32(&indevmode, 1)

					w.Header().Set("Content-Type", "text/plain")
					w.Write([]byte("OK\n"))
				case "/prod":
					log.Printf("[%s]: enabling production mode", req.RemoteAddr)
					r, err := reload(*rootdir)
					if err != nil {
						log.Printf("[%s]: reload failed: %v", req.RemoteAddr, err)
						w.WriteHeader(500)
						w.Write([]byte(fmt.Sprintf("reload failed: %v\n", err)))
					}
					resourceLock.Lock()
					resources = r
					resourceLock.Unlock()

					atomic.StoreUint32(&indevmode, 0)

					w.Header().Set("Content-Type", "text/plain")
					w.Write([]byte("OK\n"))
				case "/reload":
					log.Printf("[%s]: reloading", req.RemoteAddr)
					r, err := reload(*rootdir)
					if err != nil {
						log.Printf("[%s]: reload failed: %v", req.RemoteAddr, err)
						w.WriteHeader(500)
						w.Write([]byte(fmt.Sprintf("reload failed: %v\n", err)))
						return
					}

					resourceLock.Lock()
					resources = r
					resourceLock.Unlock()

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

	var isfancyplace func(string) bool

	if *fromdisk == "" {
		isfancyplace = func(string) bool { return false }
	} else {
		matchstr := path.Join(*rootdir, *fromdisk) + "/"
		matchlen := len(matchstr)
		isfancyplace = func(p string) bool {
			if matchlen > len(p) {
				return false
			}
			return p[:matchlen] == matchstr
		}
	}

	handler := func(w http.ResponseWriter, req *http.Request) {

		log.Printf("[%s]: \"%s %v %s\"", req.RemoteAddr, req.Method, req.URL, req.Proto)

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
			return
		}

		// Fetch resource
		p := path.Join(*rootdir, path.Clean(req.URL.Path))

		devreq := atomic.LoadUint32(&indevmode) == 1

		var r *resource
		var exists bool
		if isfancyplace(p) {
			if s, err := os.Stat(p); err != nil || s.IsDir() {
				goto doom
			}
			r = &resource{}
			if err = r.load(p); err != nil {
				r = nil
				goto doom
			}
			r.cache = 0
			exists = true
		} else {
			if devreq {
				resourceLock.Lock()
				log.Printf("[%s]: reloading", req.RemoteAddr)
				r, err := reload(*rootdir)
				if err != nil {
					resourceLock.Unlock()
					log.Printf("[%s]: reload failed: %v", req.RemoteAddr, err)
					w.WriteHeader(500)
					w.Write([]byte(fmt.Sprintf("<!doctype html><html><body><h1>500 Internal Server Error</h1><p>%v</p></body></html>\n", err)))
					return
				}
				resources = r
				resourceLock.Unlock()
			}

			resourceLock.RLock()
			r, exists = resources[p]
			resourceLock.RUnlock()
		}

	doom:
		if !exists {
			w.WriteHeader(404)
			w.Write([]byte("<!doctype html><html><body><h1>404 File Not Found</h1></body></html>\n"))
			return
		}

		cacheResponse := false

		ifModifiedSince := req.Header.Get("If-Modified-Since")
		ifNoneMatch := req.Header.Get("If-None-Match")
		if ifNoneMatch != "" {
			cacheResponse = ifNoneMatch == r.hash
		} else if ifModifiedSince != "" {
			imsdate, err := time.Parse(time.RFC1123, ifModifiedSince)
			cacheResponse = err == nil && imsdate.After(r.loaded)
		}

		usegzip := r.gzip != nil && strings.Contains(req.Header.Get("Accept-Encoding"), "gzip")
		var b []byte
		if usegzip {
			b = r.gzip
		} else {
			b = r.body
		}
		// Set headers
		h := w.Header()
		if r.cnttype != "" {
			h.Set("Content-Type", r.cnttype)
		}

		now := time.Now().Format(time.RFC1123)
		h.Set("Etag", r.hash)
		h.Set("Date", now)
		h.Set("Expires", now)
		h.Set("Last-Modified", r.loaded.Format(time.RFC1123))
		h.Set("Vary", "Accept-Encoding")

		var cc string
		if r.cache == 0 || devreq {
			cc = fmt.Sprintf("public, max-age=0, no-cache")
		} else {
			cc = fmt.Sprintf("public, max-age=%.0f", r.cache.Seconds())
		}
		h.Set("Cache-Control", cc)

		if cacheResponse {
			w.WriteHeader(304)
			return
		}

		if usegzip {
			h.Set("Content-Encoding", "gzip")
		}
		h.Set("Content-Length", fmt.Sprintf("%d", len(b)))

		if head {
			w.WriteHeader(200)
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
