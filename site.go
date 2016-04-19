package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"os"
	"path"
	"time"
)

type resource struct {
	body           []byte
	gzip           []byte
	bodyReadCloser io.ReadCloser
	permitGZIP     bool

	loaded  time.Time
	cnttype string
	cache   string

	// I don't like people. They claim that entity tags should be different on
	// different content-encodings. The spec states that the tag is for an
	// "entity variant", but does not specify an entity variant in further
	// detail. Why do I need to distinguish between encodings that have no
	// effect on the content? The idea is that the browser can tell me that it
	// has a certain resource, and it is curious if this resource is still
	// relevant. Why would I care if it previously downloaded it in plain or
	// gzipped variant? It's an encoding, it does not affect the content at all.
	// I hate you, internet.
	hash  string
	ghash string

	path     string
	fromDisk bool
	config   *SiteConfig
}

func (r *resource) updateTagCompress() {
	buf := new(bytes.Buffer)
	gz := gzip.NewWriter(buf)
	gz.Write(r.body)
	gz.Close()
	r.gzip = buf.Bytes()

	h := sha256.Sum256(r.body)
	r.hash = hex.EncodeToString(h[:])
	h = sha256.Sum256(r.gzip)
	r.ghash = hex.EncodeToString(h[:])

	r.update()
}

func (r *resource) update() {
	var (
		cache        time.Duration
		cacheconf    = r.config.Cache
		ext          = path.Ext(r.path)
		compressconf = r.config.Compression
		mincompsize  = compressconf.MinSize
	)

	r.cnttype = mime.TypeByExtension(ext)

	if compressconf.MinSize == 0 {
		mincompsize = DefaultSiteConfig.Compression.MinSize
	}

	// Evaluate cache time.
	if (!r.fromDisk && !cacheconf.NoCacheFromMem) ||
		(r.fromDisk && !cacheconf.NoCacheFromDisk) {

		if x, exists := cacheconf.CacheTimes[ext]; exists {
			cache = x.Duration
		} else {
			cache = cacheconf.DefaultCacheTime.Duration
		}
	}

	if cache == 0 {
		r.cache = "public, max-age=0, no-cache"
	} else {
		r.cache = fmt.Sprintf("public, max-age=%.0f", cache.Seconds())
	}

	// Evaluate compression.
	if (r.fromDisk && compressconf.NoCompressFromDisk) ||
		(!r.fromDisk && compressconf.NoCompressFromMem) ||
		(r.body != nil && len(r.body) < mincompsize) {
		return
	}

	for _, v := range compressconf.Blacklist {
		if ext == v {
			return
		}
	}

	if r.gzip != nil && float64(len(r.gzip))*1.1 >= float64(len(r.body)) {
		// Clear gzip.
		r.gzip = nil
		return
	}

	r.permitGZIP = true
}

type cache struct {
	plain []byte
	gzip  []byte
	hash  string
	ghash string
}

type site struct {
	http   map[string]*resource
	https  map[string]*resource
	config *SiteConfig
}

func (s *site) addResource(diskpath, sitepath string, cachemap map[string]*cache, http, https bool) error {
	fi, err := os.Stat(diskpath)
	if err != nil {
		return err
	}

	if fi.IsDir() {
		diskpath = path.Join(diskpath, s.config.General.DefaultFile)
		if fi, err = os.Stat(diskpath); err != nil || fi.IsDir() {
			// We're here because the path addResource was called with was a
			// directory, and the directory either lacked the default file, or
			// the default file was a directory as well. Not being able to
			// associate a default file with a directory is not an error, so we
			// just skip the entry.
			return nil
		}
	}

	r := &resource{
		path:   diskpath,
		config: s.config,
		loaded: fi.ModTime(),
	}

	r.body, err = ioutil.ReadFile(r.path)
	if err != nil {
		return err
	}

	h := sha256.Sum256(r.body)
	r.hash = hex.EncodeToString(h[:])

	// Check if we already have this content read so we can deduplicate it.
	if cached, exists := cachemap[r.hash]; exists {
		r.body = cached.plain
		r.gzip = cached.gzip
		r.hash = cached.hash
		r.ghash = cached.ghash
	} else {
		buf := new(bytes.Buffer)
		gz := gzip.NewWriter(buf)
		gz.Write(r.body)
		gz.Close()
		r.gzip = buf.Bytes()

		h = sha256.Sum256(r.gzip)
		r.ghash = hex.EncodeToString(h[:])

		cachemap[r.hash] = &cache{
			plain: r.body,
			gzip:  r.gzip,
			hash:  r.hash,
			ghash: r.ghash,
		}
	}

	r.update()

	if http {
		s.http[sitepath] = r
	}
	if https {
		s.https[sitepath] = r
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
