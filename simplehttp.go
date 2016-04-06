package main

import (
	"flag"
	"log"
	"net/http"
	"sync"
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

func main() {
	var err error
	flag.Parse()
	if *logfile != "" {
		log.SetOutput(&RotateWriter{Interval: 24 * time.Hour, Root: *logfile})
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

	sl := &sitelist{
		root:        *rootdir,
		defaulthost: *defaulthost,
		defErrNoSuchHost: &resource{
			body:    []byte("no such service"),
			cnttype: "text/plain",
		},
		defErrNoSuchFile: &resource{
			body:    []byte("no such file"),
			cnttype: "text/plain",
		},
	}

	sl.defErrNoSuchFile.update()
	sl.defErrNoSuchHost.update()

	if err = sl.load(); err != nil {
		log.Printf("Unable to walk files: %v", err)
		return
	}

	sl.dev(*development)

	if *cmdaddr != "" {
		go func() {
			s := &http.Server{
				Addr:         *cmdaddr,
				Handler:      http.HandlerFunc(sl.cmdhttp),
				ReadTimeout:  10 * time.Second,
				WriteTimeout: 10 * time.Second,
			}
			log.Fatal(s.ListenAndServe())
		}()
	}

	var wg sync.WaitGroup
	if *address != "" {
		wg.Add(1)
		go func() {
			s := &http.Server{
				Addr:         *address,
				Handler:      http.HandlerFunc(sl.http),
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
				Handler:      http.HandlerFunc(sl.http),
				ReadTimeout:  30 * time.Second,
				WriteTimeout: 10 * time.Minute,
			}

			log.Fatal(s.ListenAndServeTLS(*tlscert, *tlskey))
			wg.Done()
		}()
	}

	wg.Wait()
}
