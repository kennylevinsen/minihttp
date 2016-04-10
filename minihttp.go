package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	configFile  = flag.String("config", "minihttp.toml", "the config file to use")
	rootdir     = flag.String("rootdir", "", "the dir to serve from (overwrites config)")
	address     = flag.String("address", "", "address to listen on (overwrites config)")
	tlsAddress  = flag.String("tlsAddress", "", "address to listen on for TLS (overwrites config)")
	tlsCert     = flag.String("tlsCert", "", "certificate for TLS (overwrites config)")
	tlsKey      = flag.String("tlsKey", "", "key for TLS (overwrites config)")
	logFile     = flag.String("logFile", "", "file to use for logging (overwites config)")
	command     = flag.String("command", "", "address to use for command server (overwrites config)")
	development = flag.Bool("dev", false, "reload on every request (if no config set)")
)

func main() {
	var err error
	flag.Parse()

	conf, err := readServerConf(*configFile)
	if err != nil {
		if conf == nil {
			log.Printf("Cannot read configuration for server: %v", err)
			return
		}
		log.Printf("Cannot read configuration for server, using default: %v", err)
	}

	if *rootdir != "" {
		conf.Root = *rootdir
	}
	if *address != "" {
		conf.HTTP.Address = *address
	}
	if *tlsAddress != "" {
		conf.HTTPS.Address = *tlsAddress
	}
	if *tlsCert != "" {
		conf.HTTPS.Cert = *tlsCert
	}
	if *tlsKey != "" {
		conf.HTTPS.Key = *tlsKey
	}
	if *logFile != "" {
		conf.LogFile = *logFile
	}
	if *command != "" {
		conf.Command.Address = *command
	}
	if *development != false {
		conf.Development = *development
	}

	if conf.LogFile != "" {
		rw := newRotateWriter(conf.LogFile, conf.LogLines)
		defer rw.Shutdown()

		go func() {
			fmt.Fprintf(os.Stderr, "RotateWriter terminated: %v\n", rw.Serve())
		}()
		log.SetOutput(rw)
	}

	if conf.Root == "" {
		fmt.Fprintf(os.Stderr, "Missing directory to serve\n")
		flag.Usage()
		return
	}

	if conf.HTTP.Address == "" && conf.HTTPS.Address == "" {
		fmt.Fprintf(os.Stderr, "Missing address to serve\n")
		flag.Usage()
		return
	}

	if conf.HTTPS.Address != "" && (conf.HTTPS.Cert == "" || conf.HTTPS.Key == "") {
		fmt.Fprintf(os.Stderr, "Missing key/cert for tls\n")
		flag.Usage()
		return
	}

	sl := &sitelist{
		root:        conf.Root,
		defaulthost: conf.DefaultHost,
		defErrNoSuchHost: &resource{
			body:    []byte("no such service"),
			cnttype: "text/plain",
			config:  &DefaultSiteConfig,
		},
		defErrNoSuchFile: &resource{
			body:    []byte("no such file"),
			cnttype: "text/plain",
			config:  &DefaultSiteConfig,
		},
	}

	sl.defErrNoSuchFile.updateAll()
	sl.defErrNoSuchHost.updateAll()

	if err = sl.load(); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to walk files: %v\n", err)
		return
	}

	sl.dev(*development)

	if conf.Command.Address != "" {
		go func() {
			log.Printf("Starting command server at: %s", conf.Command.Address)
			s := &http.Server{
				Addr:         conf.Command.Address,
				Handler:      http.HandlerFunc(sl.cmdhttp),
				ReadTimeout:  10 * time.Second,
				WriteTimeout: 10 * time.Second,
			}
			log.Printf("Command server failure: %v", s.ListenAndServe())
		}()
	}

	var wg sync.WaitGroup
	if conf.HTTP.Address != "" {
		wg.Add(1)
		go func() {
			log.Printf("Starting HTTP server at: %s", conf.HTTP.Address)
			s := &http.Server{
				Addr:         conf.HTTP.Address,
				Handler:      http.HandlerFunc(sl.http),
				ReadTimeout:  30 * time.Second,
				WriteTimeout: 10 * time.Minute,
			}
			log.Printf("HTTP server failure: %v", s.ListenAndServe())
			wg.Done()
		}()
	}

	if conf.HTTPS.Address != "" {
		wg.Add(1)
		go func() {
			log.Printf("Starting HTTPS server at: %s", conf.HTTPS.Address)
			s := &http.Server{
				Addr:         conf.HTTPS.Address,
				Handler:      http.HandlerFunc(sl.http),
				ReadTimeout:  30 * time.Second,
				WriteTimeout: 10 * time.Minute,
			}

			log.Printf("HTTPS server failure: %v", s.ListenAndServeTLS(conf.HTTPS.Cert, conf.HTTPS.Key))
			wg.Done()
		}()
	}

	log.Printf("Terminating")
	wg.Wait()
}
