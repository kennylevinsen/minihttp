# minihttp

minihttp is a small webserver written in go. It features:

* Sane defaults. You can run entirely without a configuration file.
* Vhosts (hostname and scheme) based on directory structure, not configuration.
* Serve entirely from memory, with a per-hostname from-disk directory for big files.
* GZIP without latency for from-memory files.
* Deduplication of in-memory files.
* Cache headers (last-modified, etag, expiry), with per-site, per-file extension configuration.
* Development mode that reloads all files on every request.
* Command server that allows toggling development mode, as well as triggering reloads, which also add/remove vhosts and reloads their configuration.
* Access logs with x-forwarded-for and user-agent.
* Fast stuff.

There's no CGI. No per-path configuration. No rewrite rules. If you want something fancier, go see [Caddy](https://caddyserver.com).

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
minihttp -rootdir web -address :8080
```

(TLS can be enabled with -tlsAddress, -tlsCert and -tlsKey)

4. Try things out:
```text
$ curl http://localhost:8080
Hello from HTTP
```

And that's it!

If you want to run on port 80 on a Linux box, I would highly recommend setting the appropriate capabilities, rather than running the server as root:
```text
sudo setcap cap_net_bind_service=+ep $(which minihttp)
minihttp -rootdir web -address :80
```

### Server configuration

```text
# This is where we will server from.
root = "/srv/web"

# This is the host that will be used if the request does not provide a Host
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
    ".woff": "90d"
    ".woff2": "90d"
    ".ttf": "90d"
    ".eot": "90d"
    ".otf": "90d"
    ".jpg": "90d"
    ".png": "90d"
    ".css": "7d"

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

```text
$ curl localhost:7000/reload
OK
$ curl localhost:7000/devel
OK
$ curl localhost:7000/status
Sites (2):
    example.com (133 HTTP resources, 133 HTTPS resources)
    other.com (48 HTTP resources, 48 HTTPS resources)
Root: web
Dev mode: true
No such host: false
No such file: false
$ curl localhost:7000/prod
OK
$ curl localhost:7000/status
Sites (2):
    example.com (133 HTTP resources, 133 HTTPS resources)
    other.com (48 HTTP resources, 48 HTTPS resources)
Root: web
Dev mode: false
No such host: false
No such file: false
```
