# minihttp

minihttp is a small webserver written in go. It features:

* Zero-config vhost.
* Serve from memory, with server-wide deduplication by hash.
* Files gzipped ahead of time for zero-delay compressed responses.
* Sane cache-headers by default, or configurable per host per file-extension.
* Development mode to reload files on every request.
* Command-server for runtime-reload, status reports, and development mode toggling
* Decent access logs with primitive X-Forwarded-For handling and user agents.
* Sane defaults. Ain't nobody got time for config, so two parameters is all it takes ot start (4 for TLS).
* Per-site from-disk folder for heavy assets or quick filesharing (with independent cache and compression settings).
* Fast stuffs.

There's no CGI. No per-path configuration. No rewrite rules. If you want something fancier, go see [Caddy](https://caddyserver.com).

The command server thing is totally cool. Added a vhost? Removed one? Changed your site? `curl localhost:7000/reload`. Maybe even `curl localhost:7000/status` to see how much memory you're using post-deduplication on your files, and what vhosts are enabled.

### How to use

1. Get minihttp:
```text
go get github.com/joushou/minihttp
```

2. Make directory structure:
```text
mkdir -p web/example.com/http
mkdir -p web/example.com/https
mkdir -p web/example.com/common
echo "Hello from HTTP" > web/example.com/http/index.html
echo "Hello from HTTPS" > web/example.com/https/index.html
echo "Shared file" > web/example.com/common/shared.html
```

3. Start minihttp:
```text
# TLS can be enabled with -tlsAddress, -tlsCert and -tlsKey
minihttp -rootdir web -address :8080
```

4. Try things out:
```text
$ curl http://localhost:8080
Hello from HTTP
```

And that's it! If you want another vhost, just make another folder like the web one. You can either restart the server or use the command API to reload it (see the bottom of the README). If you want to run the server permanently, I suggest making a server configuration file, but that's up to you.

If you want to run on port 80 on a Linux box, I would highly recommend setting the appropriate capabilities, rather than running the server as root:
```text
sudo setcap cap_net_bind_service=+ep $(which minihttp)
minihttp -rootdir web -address :80
```

### Server configuration

Ain't got time for documentation, so here is a configuration file with every option set and a comment:

```text
# This is where we will server from.
root = "/srv/web"

# This is the host that will be used if the requested one is not found.
# header.
defaultHost = "example.com"

# The log/access file.
logFile = "/var/log/minihttp/minihttp.log"

# How many lines to write before the log is rotated and gzipped.
logLines = 8192

# Whether or not to start in development mode.
development = false

# The address for HTTP operation.
[http]
    address = ":80"

# The address, certificate and key for TLS operation.
[https]
    address = ":443"
    cert = "cert.pem"
    key = "key.pem"

# The address to serve the command interface on.
[command]
    address = ":7000"
```

Load with:
```text
minihttp -config config.toml
```

### Site configuration

Like above, all options set and comments:

```text
[general]
    # This disables default files for this repo.
    noDefaultFile = false

    # The default file.
    defaultFile = "index.html"

    # The from-disk folder prefix. If a URL matches this prefix, the file will
    # be fetched from the fancy/ folder of the site. Note that the /f/ will be
    # included, so /f/hello will be fetched from site/fancy/f/hello.
    fancyFolder = "/f/"

[cache]
    # This flips the cache headers to be cache-busting for memory content.
    noCacheFromMem = false

    # This flips the cache headers to be cache-busting for disk content.
    noCacheFromDisk = false

    # This is the default cache time to advertise for content that does not have
    # a specific cache time set.
    defaultCacheTime = "1h"

# Specific cache times. The syntax is that which time.ParseDuration accepts.
[cache.cacheTimes]
    ".woff" ="90d"
    ".woff2" = "90d"
    ".ttf" = "90d"
    ".eot" = "90d"
    ".otf" = "90d"
    ".jpg" = "90d"
    ".png" = "90d"
    ".css" = "7d"

[compression]
    # This disables GZIP compression for memory content.
    noCompressFromMem = false

    # This disables GZIP compression for disk content. Disabling this lowers CPU
    # load if there are many requests to from-disk files, but increases
    # bandwidth consumption. Memory content does not have this issue.
    noCompressFromDisk = true

    # The minimum file size a file must have to even consider compression.
    minSize = 256

    # The minimum size reduction a file must see after compression to use the
    # compressed result.
    minRatio = 1.1

    # Files that will never be compressed. Recompressing compressed files is
    # mostly just a waste of cycles for both the server and client.
    blacklist = [".jpg", ".zip", ".gz", ".tgz"]
```

Given the previously mentioned file structure, put the file in web/example.com/config.toml and reload the web server.

### Error files

The server includes a hardcoded 404 page for when files don't exist or can't be read, as well as a 500 page for when a hostname is not known to the server.

A 404.html and 500.html can be put in the rootdir ("web/404.html" and "web/500.html" in the example folder above), which will replace the builtin variants. Furthermore, a 404.html can be put in the site folder ("web/example.com/404.html" in the example folder above), which will apply only to that site.

### Command API

Bah, I'll just show you this too:

```text
$ # Reload vhosts, their configurations and files.
$ curl localhost:7000/reload
OK

$ # Enable development mode.
$ curl localhost:7000/devel
OK

$ # Check the server status.
$ curl localhost:7000/status
Sites (5):
	example.com (125 HTTP resources, 125 HTTPS resources)
	other.com (133 HTTP resources, 133 HTTPS resources)

Settings:
	Root: /somewhere/web
	Dev mode: true
	No such host: true
	No such file: true

Stats:
	Total plain file size: 24950204
	Total gzip file size   12344953
	Total files:           225

$ # Disable development mode (production mode).
$ curl localhost:7000/prod
OK

$ # Check status to see the change.
$ curl localhost:7000/status
Sites (5):
	example.com (125 HTTP resources, 125 HTTPS resources)
	other.com (133 HTTP resources, 133 HTTPS resources)

Settings:
	Root: /somewhere/web
	Dev mode: false
	No such host: true
	No such file: true

Stats:
	Total plain file size: 24950204
	Total gzip file size   12344953
	Total files:           225
```

(Yes, I typed in some random numbers. If you want real numbers, run it yourself!)
