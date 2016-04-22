package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	configFile  = flag.String("config", "minihttp.toml", "the config file to use")
	rootdir     = flag.String("rootdir", "", "the dir to serve from")
	address     = flag.String("address", "", "address to listen on")
	tlsAddress  = flag.String("tlsAddress", "", "address to listen on for TLS")
	tlsCert     = flag.String("tlsCert", "", "certificate for TLS")
	tlsKey      = flag.String("tlsKey", "", "key for TLS")
	logFile     = flag.String("logFile", "", "file to use for logging (overwites config)")
	command     = flag.String("command", "", "address to use for command server")
	development = flag.Bool("dev", false, "reload on every request (if no config set)")
	quiet       = flag.Bool("quiet", false, "disable logging")
)

func main() {
	var err error
	flag.Parse()

	conf, err := readServerConf(*configFile)
	if err != nil {
		if conf == nil {
			fmt.Printf("Cannot read configuration for server: %v\n", err)
			return
		}
		fmt.Printf("Cannot read configuration for server, using default: %v\n", err)
	}

	// Apply command-line arguments over the provided configuration
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
	if *development {
		conf.Development = *development
	}

	var logger func(string, ...interface{}) = (&Logger{Writer: os.Stderr}).Printf

	if *quiet {
		logger = func(string, ...interface{}) {}
	} else if conf.LogFile != "" {
		rw, err := NewRotateWriter(conf.LogFile, conf.LogLines)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not initialize RotateWriter: %v\n", err)
			return
		}
		defer rw.Shutdown()

		go func() {
			fmt.Fprintf(os.Stderr, "RotateWriter terminated: %v\n", rw.Serve())
		}()

		logger = (&Logger{Writer: rw}).Printf
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

	// Load sitelist
	sl := &sitelist{
		root:        conf.Root,
		defaulthost: conf.DefaultHost,
		logger:      logger,
	}

	if err = sl.load(); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to walk files: %v\n", err)
		return
	}

	sl.dev(*development)

	// Start your engines!
	if conf.Command.Address != "" {
		go func() {
			logger("Starting command server at: %s\n", conf.Command.Address)
			s := &http.Server{
				Addr:         conf.Command.Address,
				Handler:      http.HandlerFunc(sl.cmdhttp),
				ReadTimeout:  10 * time.Second,
				WriteTimeout: 10 * time.Second,
			}
			logger("Command server failure: %v\n", s.ListenAndServe())
		}()
	}

	var wg sync.WaitGroup
	if conf.HTTP.Address != "" {
		wg.Add(1)
		go func() {
			logger("Starting HTTP server at: %s\n", conf.HTTP.Address)
			s := &http.Server{
				Addr:         conf.HTTP.Address,
				Handler:      http.HandlerFunc(sl.http),
				ReadTimeout:  30 * time.Second,
				WriteTimeout: 10 * time.Minute,
			}
			logger("HTTP server failure: %v\n", s.ListenAndServe())
			wg.Done()
		}()
	}

	if conf.HTTPS.Address != "" {
		wg.Add(1)
		go func() {
			logger("Starting HTTPS server at: %s\n", conf.HTTPS.Address)
			s := &http.Server{
				Addr:         conf.HTTPS.Address,
				Handler:      http.HandlerFunc(sl.http),
				ReadTimeout:  30 * time.Second,
				WriteTimeout: 10 * time.Minute,
			}

			logger("HTTPS server failure: %v\n", s.ListenAndServeTLS(conf.HTTPS.Cert, conf.HTTPS.Key))
			wg.Done()
		}()
	}

	wg.Wait()
	logger("Terminated\n")
}
