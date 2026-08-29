<p align="center">
  <img src="unraid/flexdav.png" width="96" alt="">
</p>

# FlexDav

A read-only WebDAV bridge in front of a remote media server. Works with Plex.

It serves a remote library as a **read-only WebDAV endpoint**, so it can be
mounted with rclone and combined with local directories through mergerfs. No
copy of the media is kept: reads are proxied to Plex as HTTP range requests, so
seeking works and nothing is downloaded ahead of time.

A library that is shared with you becomes an ordinary directory tree that any
tool can read.

```
/Movies/The Cartographer (2018) Bluray-1080p x264-GRP.mkv
/TV Shows/Harbour Lights/Season 2/Harbour Lights - S02E04.mkv
```

It runs with **your own per-server token**, against a library its owner has
shared with you, and it is read-only: there is no write path, no second copy of
anything, and the media itself is never touched. It is a client for the Plex
HTTP API, in the same sense that Tautulli or plexapi are.

FlexDav is an independent project. It is **not affiliated with, endorsed by or
sponsored by Plex Inc.**, and it is not a Plex product. "Plex" appears here only
to name the API it speaks, and is a trademark of its owner.

## Why

The usual way to use a shared Plex library outside the Plex client is to
download from it and store a second copy. This does the opposite: one process
translates the Plex HTTP API into WebDAV, rclone mounts that, and mergerfs
unions it with a writable local branch so your own subtitles can sit next to
remote media in a single tree.

## What works

Verified against two real shared servers, one of them sixteen sections and
hundreds of thousands of items:

- Section listing, per-section filtering, show/season/episode traversal.
- Real release names, because Plex exposes `Part.file`, which is what Radarr,
  Sonarr and Bazarr parse.
- Ranged reads return real media: the first bytes of an MKV are `1a45dfa3`.
  Seeking ten gigabytes into a file returns at once instead of streaming
  through everything before it.
- Two servers at once, either merged into one tree or with one mirrored exactly
  and the other standing in only when it is down.
- Playback-grade throughput through the full rclone + mergerfs stack once
  tuned. The rclone flags matter far more than the code does, see
  [Throughput](#throughput).
- The whole stack survives a reboot.

The first listing of a section is the slow one; everything after it is served
from cache. Cost grows with the number of entries, and show sections are far
worse than film sections of the same size, because a show has to be walked
season by season while a film is a single entry.

## Install

Three ways, same container underneath. In all of them the port stays on
`127.0.0.1`: the endpoint hands the whole library to whoever reaches it.

**Where this has actually been run:** one Unraid host, with Docker, the rclone
plugin and the mergerFS plugin. Nothing in the bridge is tied to Unraid (it is
a static Go binary in a container, and the mount and union layers are ordinary
Linux tools), but Unraid is the only place the whole stack has been exercised,
so treat anything else as untested rather than unsupported.

### docker run

```bash
cp .env.example .env && chmod 600 .env   # then fill in PLEX_BASE_URL and PLEX_TOKEN
docker run -d --name flexdav --restart unless-stopped \
  --env-file .env -p 127.0.0.1:8099:8080 ghcr.io/phiivas/flexdav:latest
```

To build it yourself instead of pulling:

```bash
git clone https://github.com/phiivas/flexdav.git && cd flexdav
docker build -t flexdav:dev .
```

### docker compose

`docker-compose.yml` in this repository builds from source by default; switch
the two commented lines to pull `ghcr.io/phiivas/flexdav:latest` instead.

```bash
cp .env.example .env && chmod 600 .env
docker compose up -d
```

### Unraid

`unraid/flexdav.xml` is a ready template. Put it on the flash drive:

```bash
curl -o /boot/config/plugins/dockerMan/templates-user/my-flexdav.xml \
  https://raw.githubusercontent.com/phiivas/flexdav/main/unraid/flexdav.xml
```

Then Docker, Add Container, and pick **FlexDav** from the "Select a template"
list at the top.

Before starting it, create the settings file the template points at:

```bash
mkdir -p /mnt/user/appdata/flexdav
curl -o /mnt/user/appdata/flexdav/.env \
  https://raw.githubusercontent.com/phiivas/flexdav/main/.env.example
chmod 600 /mnt/user/appdata/flexdav/.env
```

and fill in `PLEX_BASE_URL` and `PLEX_TOKEN`.

The template carries no secrets on purpose: templates sit on the flash drive
in plain text. It also carries no `Variable` entries for the tuning knobs, and
that is not an oversight. Docker keeps the **last** occurrence of a repeated
environment variable and template variables are appended after `--env-file`,
so a template variable sitting at its default silently overrides `.env`. An
empty `PLEX_SECTIONS` arriving that way means "expose every section". Keep all
tuning in `.env`.

## About the token

Use the **per-server** token for the library you want to serve, not your Plex
account token. The account token reaches every server shared with you, and
whatever it does looks like you to all of them. Write it straight into `.env`,
never into a shell command line or a URL query string.

Connectivity is checked at startup but not required, because a remote server
can be unreachable for minutes at a stretch. Exiting on a failed check would
mean the mount above it never comes up either, so a reboot during an outage
would leave the whole stack down until somebody noticed. It starts anyway and
reads retry on their own.

## First check

Check it by hand before mounting anything:

```bash
curl -u plex:change-me -X PROPFIND -H 'Depth: 1' http://127.0.0.1:8099/
```

Range support is what makes playback seekable, so confirm a 206 comes back:

```bash
curl -u plex:change-me -r 0-1023 -o /dev/null -D - 'http://127.0.0.1:8099/Movies/'
```

## Configuration

| Variable | Required | Default | Notes |
|---|---|---|---|
| `PLEX_BASE_URL` | yes | | Base URL of the Plex server |
| `PLEX_TOKEN` | yes | | Per-server token |
| `PLEX_SECTIONS` | no | all | Comma-separated library titles to expose |
| `PLEX_STREAMS` | no | `4` | Concurrent ranged GETs per open file |
| `PLEX_CHUNK_MIB` | no | `8` | Largest single ranged GET. `32` measured faster |
| `PLEX_HTTP2` | no | off | Allow HTTP/2 for this server. Measure, see below |
| `PLEX_NAME` | no | host | Label used in log lines |
| `LISTEN_ADDR` | no | `:8080` | |
| `WEBDAV_USER` / `WEBDAV_PASS` | no | none | If unset, the endpoint is unauthenticated |
| `CACHE_TTL` | no | `30m` | Fallback expiry for cached listings |
| `REFRESH_INTERVAL` | no | `5m` | How often Plex is polled for changed sections |
| `RCLONE_RC_URL` | no | | rclone remote control, to expire only what changed |
| `PLEX_LOCAL_URL` / `PLEX_LOCAL_TOKEN` | no | | A local Plex to ask for targeted rescans |
| `PLEX_MIRROR_PRIMARY` | no | off | Serve the first server's tree exactly; others are failover only |
| `PPROF_ADDR` | no | off | Enable net/http/pprof on this address |

Additional servers use the same names with a number: `PLEX2_BASE_URL`,
`PLEX2_TOKEN`, `PLEX2_STREAMS` and so on, scanned until one is missing. The
first server to hold a title is the one served, so the numbering is the
preference order.

Every tuning knob is per server on purpose, because the right values are a
property of the server rather than a universal answer. One may be fastest on
HTTP/1.1 with four streams and another on HTTP/2 with eight, and carrying one
server's numbers over to another can cost most of the throughput.

### Picking up new content

Every `REFRESH_INTERVAL` the bridge lists sections (about 11 KB, under a
second) and compares each `updatedAt` with the last one seen. Only sections
Plex reports as changed have their cached listings dropped, so this stays cheap
no matter how large the library is. Re-listing everything on a timer would mean
minutes of work per cycle.

`CACHE_TTL` is then only a backstop and can be long. The mount's own directory
cache adds a delay on top:

```
REFRESH_INTERVAL=5m + rclone --dir-cache-time 10m  ->  new content within ~15m
```

If `RCLONE_RC_URL` is set, the bridge tells the mount to forget exactly the
directories that changed, which is what lets the mount hold its cache for a day
instead of ten minutes.

## Mounting

```bash
rclone config create flexdav webdav url http://127.0.0.1:8099 vendor other user plex pass change-me
```

```bash
rclone mount flexdav: /mnt/mounts/flexdav --read-only --allow-other \
  --dir-cache-time 24h --poll-interval 0 \
  --vfs-cache-mode full --cache-dir /var/cache/rclone \
  --vfs-cache-max-size 50G --vfs-cache-max-age 24h \
  --vfs-read-chunk-size 0 \
  --buffer-size 128M --timeout 1800s --attr-timeout 10m --daemon
```

`--vfs-read-chunk-size 0` is the single most important flag here, see below.
`--vfs-cache-mode full` matters too: Plex scanning a file seeks all over it, and
without a cache every seek becomes a fresh request.

### Union with a writable branch

```bash
mergerfs -o allow_other,category.create=ff,func.getattr=newest,cache.files=partial,dropcacheonclose=true,minfreespace=0,fsname=plexmerged \
  /mnt/mounts/flexdav=RO:/mnt/data/subs=RW /mnt/mounts/merged
```

`category.create=ff` only considers branches that allow creation, so every
write goes to the local branch and the remote one is never touched.
`func.getattr=newest` makes a freshly written subtitle win over a stale entry.

Point Bazarr, or whatever writes subtitles, at the merged path.

## Throughput

**Read these as ratios, not as speeds you will get.** Every figure below is one
setup reading from one server. Bandwidth, latency and the load on the machine
at the far end decide the absolute numbers, and none of those are properties
of this program. What carries over to another setup is the ordering: which
layer costs what, and which setting beats which.

Measured with 256 MB reads, one layer added at a time, each row against a
direct connection to the same server:

| Path | Relative |
|---|---|
| Direct to the provider, one connection | 1.00 |
| Through the bridge (curl, no mount) | 0.87 |
| Through rclone alone | 0.28 |
| Through rclone + mergerfs | 0.24 |

The bridge costs about 13 %. mergerfs costs nothing measurable. rclone's VFS
was eating 3.4x, and turning its chunking off is what got it back:

| rclone setting | Relative |
|---|---|
| `--vfs-read-chunk-size 32M` | 1.00 |
| `--vfs-read-chunk-size 128M` | 1.17 |
| `--vfs-read-ahead 512M` | 1.07 |
| `--vfs-read-chunk-size 0`, cache off | 2.8 |
| **`--vfs-read-chunk-size 0`, cache full** | **2.4-3.0** |

Chunking makes rclone issue a fresh ranged GET per chunk, and every one of
those restarts the bridge's read pipeline from a cold ramp. With chunking off
it opens one request and streams it.

Chunk size inside the bridge, same files, configs alternated round robin so a
change in the provider's mood hits all of them equally. Each round is scaled
to its own 8 MiB run, so only the comparison inside a row means anything:

| Round | 4 x 8 MiB | 4 x 32 MiB | 4 x 64 MiB | 6 x 32 MiB |
|---|---|---|---|---|
| 1 | 1.00 | 1.06 | 1.31 | 0.79 |
| 2 | 1.00 | 1.77 | 2.29 | 0.62 |
| 3 | 1.00 | 1.35 | 1.33 | 1.53 |
| 4 | 1.00 | 2.24 | 0.70 | 3.08 |
| mean | 1.00 | **1.58** | 1.47 | 1.46 |

32 MiB beat 8 MiB in all four rounds. 64 MiB looks similar on average but has a
much worse floor: the larger the chunk, the more a single stalled fetch costs,
because everything behind it waits. More than four streams bought nothing.

HTTP/2 against HTTP/1.1, 32 MiB ranged reads, fresh connection each time: on
one provider HTTP/2 was **about seven times** faster. Against the other the
opposite held: HTTP/2 multiplexed all four chunk fetches onto one TCP
connection and made the read pipeline parallel in name only. Neither setting
is a safe default for an unknown server.
Measure yours.

Round-to-round spread is larger than most of the differences being compared, so
treat any single run as suspect.

## Please be careful with somebody else's server

This is the part that is easy to get wrong, and getting it wrong is not
recoverable: the owner of the library usually sees your traffic in some form,
and a share can be withdrawn without warning.

**Use this at your own risk.** It reads a library that belongs to somebody
else, under rules only that person sets. Nothing here can promise the traffic
will be welcome, and the licence gives you no warranty of any kind. Read this
section before you point it at a shared server, not afterwards.

- **A mount reads real bytes.** One wrong rclone flag, chunked reads disabled
  in a way that makes the VFS fetch whole files, turns "Plex is indexing" into
  terabytes pulled in title order, which from the other side is
  indistinguishable from a bulk download. The logs will not say so. Watch the
  byte counters, not the scan progress.
- **Do not let Plex index through the mount.** Opening every remote file to
  probe its codec is enormously expensive for the provider and slow for you. A
  much better shape is to read the remote server's own index over the API,
  build a local tree of sparse placeholder files with the correct names and
  sizes, let Plex scan those off local disk, and import codec, resolution and
  bitrate from the API rather than probing the files. A section that needs many
  hours through the mount can then build in minutes, with no file opens at all
  against the server.
- **Rate limit yourself.** Two requests per second against a shared server is
  polite. The listing paths here page at 1000 items per request precisely so
  that a large library costs tens of requests rather than tens of thousands.
- **Keep the endpoint local.** Bind it to `127.0.0.1`, set `WEBDAV_USER` and
  `WEBDAV_PASS`, and do not put it on the open internet. It is a
  no-questions-asked reader for a library that is not yours.

## Known limitations

- **Listings are fetched whole.** A section is paged in at 1000 items per
  request and cached; the first listing of a large section is slow and the
  result sits in memory.
- **Only the first Part of the first Media is exposed.** Multi-part files and
  alternate versions are ignored.
- **Disambiguation is naive.** A second `Title` becomes `Title (2)`; a real item
  already called `Title (2)` would collide.
- **No write path at all**, by design. Write methods are rejected at the door.
- **Unauthenticated unless you set `WEBDAV_USER` / `WEBDAV_PASS`.**

## Layout

```
cmd/bridge/main.go           configuration, auth, read-only guard
internal/plex/               Plex API client: sections, children, ranged part reads
internal/davfs/fs.go         webdav.FileSystem implementation
internal/davfs/catalogue.go  multi-server catalogue, merge and mirror modes
internal/davfs/chunkreader.go parallel ranged reader with mid-file reconnects
internal/plexlocal/          asks a local Plex to rescan only what changed
internal/rclonerc/           expires only the changed directories in the mount
```

Tests run against a fake Plex server, no network and no account needed:

```bash
docker run --rm -v "$PWD":/src -w /src \
  -e GOFLAGS=-mod=mod -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomod \
  golang:1.22-alpine go test ./...
```

## Prior art, and what this is not

**The design is not original.** It is the WebDAV + rclone + mergerfs pattern
established by [Zurg](https://github.com/debridmediamanager/zurg-testing) by
yowmamasita, pointed at a different kind of source: Zurg exposes a remote
provider as a WebDAV endpoint, rclone mounts it, mergerfs unions it with a
writable branch, and a media server scans the result as if it were local disk.
Thousands of people run that arrangement, which is why it was worth copying:
the shape is settled, and the only open question here was whether a Plex
library shared with you can stand in as the source. It can.

**This is not a fork and contains no code from any of them.** Zurg is
distributed as binaries rather than source, so there was nothing to copy even in
principle. Everything here was written from scratch in Go, and the only
dependency is `golang.org/x/net/webdav` (BSD-3-Clause), which supplies the
protocol handler. What the Plex side needs turned out to be quite different
anyway: paging a 99k-item section, per-server transport tuning, and a parallel
ranged reader that survives a provider dropping mid-file.

Related, in case one of them suits you better:

- [vladiiancu/plex-reshare](https://github.com/vladiiancu/plex-reshare) (MIT):
  re-shares libraries shared with you to other users on your own server, as a
  browsable HTTP directory listing through OpenResty with a Python backend. An
  index to browse, rather than a filesystem to mount and union.
- [Reaparr](https://github.com/Reaparr/Reaparr), which downloads from a shared
  library and keeps a second copy. That is the model this was built as an
  alternative to: here nothing is stored and reads are proxied on demand.

## License

MIT, see [LICENSE](LICENSE).

The icon is the "Bridge at night" emoji from
[Microsoft Fluent Emoji](https://github.com/microsoft/fluentui-emoji), used
under the MIT license; its notice is kept in
[unraid/ICON-LICENSE](unraid/ICON-LICENSE).

---

Written with [Claude Code](https://claude.com/claude-code) (Opus 5).
