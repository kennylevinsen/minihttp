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

const (
	cacheControlNoCache = "public, max-age=0, no-cache"
	cacheControlCache   = "public, max-age=%.0f"
)

func gz(b []byte) []byte {
	buf := new(bytes.Buffer)
	gz, _ := gzip.NewWriterLevel(buf, gzip.BestCompression)
	gz.Write(b)
	gz.Close()
	return buf.Bytes()
}

func hash(b []byte) string {
	// You can't address into the return value of Sum256 without putting it into
	// a variable first... GRR!
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// cache stores bodies and hashes for deduplication during sitelist reload.
type cache struct {
	body  []byte
	gbody []byte
	hash  string
	ghash string
}

type resource struct {
	path           string
	body           []byte
	gbody          []byte
	bodyReadCloser io.ReadCloser

	permitGZIP bool
	fromDisk   bool
	cache      string
	cnttype    string
	loaded     time.Time
	config     *SiteConfig

	hash  string
	ghash string
}

func (r *resource) updateTagCompress() {
	r.hash = hash(r.body)
	r.gbody = gz(r.body)
	r.ghash = hash(r.gbody)
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

	if r.cnttype = mime.TypeByExtension(ext); r.cnttype == "" {
		// Meh.
		r.cnttype = "text/plain; charset=utf-8"
	}

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
		r.cache = cacheControlNoCache
	} else {
		r.cache = fmt.Sprintf(cacheControlCache, cache.Seconds())
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

	// If we know the size of the gzipped content already, we can evaluate if
	// the gzip variant is worth the effort. If I math'd this right, then the
	// threshold is a 10% size improvement. One could argue that ANY network
	// benefit is worth persuing, but if the benefit is less than 10%, the
	// network benefit is negligible, and the server is basically just holding
	// the file in memory twice.
	if r.gbody != nil && float64(len(r.gbody))*1.1 >= float64(len(r.body)) {
		// Clear gbody. In case no resources decide to store the gzipped entity
		// variant, this allows for it to be garbage collected.
		r.gbody = nil
		return
	}

	r.permitGZIP = true
}

// site represents two sets of resources (one for HTTP, one for HTTPS) and a
// related configuration. A site and its resource sets must not be mutated once
// added to a sitelist, as access to them is intentionally not locked. Reloading
// a must happen by replacing the site under sitelists' siteLock.
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

	body, err := ioutil.ReadFile(diskpath)
	if err != nil {
		return err
	}

	r := &resource{
		body:   body,
		hash:   hash(body),
		path:   diskpath,
		config: s.config,
		loaded: fi.ModTime(),
	}

	// Check if we already have this content read so we can deduplicate it.
	if cached, exists := cachemap[r.hash]; exists {
		r.body = cached.body
		r.hash = cached.hash
		r.gbody = cached.gbody
		r.ghash = cached.ghash
	} else {
		r.gbody = gz(r.body)
		r.ghash = hash(r.gbody)

		cachemap[r.hash] = &cache{
			body:  r.body,
			gbody: r.gbody,
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
