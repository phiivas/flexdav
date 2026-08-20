// Command bridge serves a remote Plex library as a read-only WebDAV
// endpoint, so it can be mounted with rclone and combined with local
// directories (Bazarr subtitles, for instance) through mergerfs.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	_ "net/http/pprof"

	"github.com/phiivas/flexdav/internal/davfs"
	"github.com/phiivas/flexdav/internal/plex"
	"github.com/phiivas/flexdav/internal/plexlocal"
	"github.com/phiivas/flexdav/internal/rclonerc"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// writeMethods are rejected outright: the backing filesystem is
// read-only, and failing at the door gives a clearer error than an
// ErrReadOnly surfacing from deep inside the WebDAV handler.
var writeMethods = map[string]bool{
	"PUT":       true,
	"DELETE":    true,
	"MKCOL":     true,
	"MOVE":      true,
	"COPY":      true,
	"PROPPATCH": true,
	"LOCK":      true,
	"UNLOCK":    true,
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	// A profiler, off unless asked for. Reasoning from the outside about
	// why four open connections move only a quarter of what four curls do
	// produced hypotheses and no answers; this shows where the goroutines
	// actually wait.
	if addr := os.Getenv("PPROF_ADDR"); addr != "" {
		runtime.SetBlockProfileRate(int(time.Millisecond))
		runtime.SetMutexProfileFraction(100)
		go func() {
			log.Printf("pprof on %s", addr)
			log.Print(http.ListenAndServe(addr, nil))
		}()
	}

	var (
		listen   = env("LISTEN_ADDR", ":8080")
		user     = os.Getenv("WEBDAV_USER")
		pass     = os.Getenv("WEBDAV_PASS")
		sections []string
	)
	if v := os.Getenv("PLEX_SECTIONS"); v != "" {
		sections = strings.Split(v, ",")
	}

	cacheTTL := 30 * time.Minute
	if v := os.Getenv("CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("CACHE_TTL %q is not a duration (try 30m): %v", v, err)
		}
		cacheTTL = d
	}

	sources := readSources()
	if len(sources) == 0 {
		log.Fatal("PLEX_BASE_URL and PLEX_TOKEN are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Reaching Plex is checked, not required. These providers drop out
	// for ten to twenty-five minutes at a time, and refusing to start
	// during one of those windows is worse than serving errors: the
	// mount above never comes up either, so a reboot at an unlucky
	// moment leaves the whole stack dead until a human notices. Reads
	// retry on their own.
	for _, s := range sources {
		if err := s.Client.Ping(ctx); err != nil {
			log.Printf("WARNING: cannot reach %s (%s) right now: %v", s.Name, s.Client.BaseURL(), err)
			log.Print("starting anyway; its titles will be missing until it comes back")
		} else {
			log.Printf("connected to %s at %s", s.Name, s.Client.BaseURL())
		}
		log.Printf("%s: %d parallel streams, up to %d MiB per chunk", s.Name, s.Streams, s.MaxChunk>>20)
	}
	if len(sources) > 1 {
		// Which mode is running decides what is in the tree at all, so it
		// has to be visible in the log rather than inferred from a count.
		if os.Getenv("PLEX_MIRROR_PRIMARY") != "" {
			log.Printf("mirroring %s exactly; the other %d server(s) only stand in when it is down",
				sources[0].Name, len(sources)-1)
		} else {
			log.Printf("merging %d servers; %s wins when the same title is on more than one",
				len(sources), sources[0].Name)
		}
	}

	if len(sections) > 0 {
		log.Printf("exposing only sections: %s", strings.Join(sections, ", "))
	} else {
		log.Print("exposing all library sections (set PLEX_SECTIONS to narrow this)")
	}

	refresh := 5 * time.Minute
	if v := os.Getenv("REFRESH_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			log.Fatalf("REFRESH_INTERVAL %q is not a positive duration (try 5m)", v)
		}
		refresh = d
	}

	// When a mount above us exposes rclone's remote control, tell it to
	// forget exactly the sections that changed. That is what lets the
	// mount hold its directory cache for a day instead of ten minutes:
	// re-listing a 99k-item section every ten minutes is ruinous, and
	// without an explicit poke a long cache would hide new episodes.
	rc := rclonerc.New(os.Getenv("RCLONE_RC_URL"))
	local := plexlocal.New(os.Getenv("PLEX_LOCAL_URL"), os.Getenv("PLEX_LOCAL_TOKEN"))
	if rc != nil {
		log.Printf("will notify the rclone mount at %s when sections change", rc.Addr())
	}
	if local != nil {
		log.Printf("will ask the local Plex at %s to rescan changed directories", os.Getenv("PLEX_LOCAL_URL"))
	}

	var onChange func(context.Context, []string)
	if rc != nil || local != nil {
		onChange = func(ctx context.Context, titles []string) {
			// Order matters. The mount has to drop its cached listing
			// before Plex looks, or Plex scans the old view and
			// concludes nothing changed, which is worse than not
			// scanning at all: it will not look again until the next
			// change.
			if err := rc.Forget(ctx, titles); err != nil {
				// Not fatal: the mount's own expiry still catches up.
				log.Printf("flexdav: %v", err)
			}
			if err := local.ScanSections(ctx, titles); err != nil {
				// Also not fatal. Most remote sections have no local
				// library at all, which this reports every time.
				log.Printf("flexdav: %v", err)
			}
		}
	}

	fsys := davfs.NewMulti(sources, davfs.Options{
		CacheTTL: cacheTTL,
		Sections: sections,
		OnChange: onChange,
	})

	// Build the catalogue in the background rather than before serving.
	// It takes minutes against libraries this size, and a container that
	// answers nothing for minutes looks dead to everything above it.
	// Requests arriving meanwhile block until it lands, which is the
	// honest answer: an empty tree would read to a scanner as "every
	// file was deleted".
	go func() {
		for attempt := 1; ; attempt++ {
			start := time.Now()
			err := fsys.Build(context.Background())
			switch {
			case err == nil:
				log.Printf("catalogue built in %s", time.Since(start).Round(time.Second))
				return
			case errors.Is(err, davfs.ErrPartialCatalogue):
				// Serving is already happening with what came back.
				// Keep rebuilding until the missing server answers,
				// because leaving its whole library out looks to the
				// scanner above like every one of its files was
				// deleted.
				log.Printf("catalogue is incomplete after %s, a server was unreachable; will rebuild",
					time.Since(start).Round(time.Second))
			case errors.Is(err, davfs.ErrStaleCatalogue):
				// Complete and already being served, but part of it is
				// the previous attempt's listing. Keep going until one
				// build sees every section first hand: this goroutine is
				// the only thing that ever does a full build, and a
				// section added while the server was away would
				// otherwise stay invisible for the life of the process.
				log.Printf("catalogue built in %s from fresh and kept listings; will build again",
					time.Since(start).Round(time.Second))
			default:
				log.Printf("catalogue build failed (attempt %d): %v", attempt, err)
			}
			// Wait longer than a typical outage before trying again.
			time.Sleep(2 * time.Minute)
		}
	}()

	// Watch for new episodes and titles. This only re-lists sections
	// Plex reports as changed, so it stays cheap regardless of how big
	// the libraries are.
	go fsys.Watch(context.Background(), refresh)
	log.Printf("checking for new content every %s (listings otherwise cached %s)", refresh, cacheTTL)

	handler := &webdav.Handler{
		FileSystem: fsys,
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}

	if user == "" || pass == "" {
		log.Print("WARNING: WEBDAV_USER/WEBDAV_PASS unset, endpoint is unauthenticated. " +
			"Anyone who can reach it can read the whole library. Do not expose it beyond the host.")
	}

	log.Printf("listening on %s", listen)
	srv := &http.Server{
		Addr:              listen,
		Handler:           guard(user, pass, handler),
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// readSources reads every configured upstream server, in priority
// order: PLEX_* first, then PLEX2_*, PLEX3_* and so on until one is
// missing. The first one to hold a title is the one served, so the
// numbering is the preference.
//
// Every setting is per server on purpose. The right ones turned out to
// be a property of the provider rather than a universal answer: one of
// these wants HTTP/1.1 and four streams, the other HTTP/2 and eight,
// and using one server's numbers against the other cost most of the
// throughput.
func readSources() []*davfs.Source {
	var out []*davfs.Source
	for i := 1; ; i++ {
		prefix := "PLEX"
		if i > 1 {
			prefix = "PLEX" + strconv.Itoa(i)
		}
		base := strings.TrimRight(os.Getenv(prefix+"_BASE_URL"), "/")
		token := os.Getenv(prefix + "_TOKEN")
		if base == "" || token == "" {
			return out
		}

		client := plex.NewClient(base, token)
		if os.Getenv(prefix+"_HTTP2") != "" {
			client = plex.NewClientHTTP2(base, token)
			log.Printf("%s: HTTP/2 enabled", prefix)
		}
		out = append(out, &davfs.Source{
			Name:     sourceName(prefix, base),
			Client:   client,
			Streams:  intEnv(prefix+"_STREAMS", 4),
			MaxChunk: int64(intEnv(prefix+"_CHUNK_MIB", 8)) << 20,
		})
	}
}

// sourceName prefers an explicit label, falling back to the host so log
// lines say something recognisable rather than "source 2".
func sourceName(prefix, base string) string {
	if n := os.Getenv(prefix + "_NAME"); n != "" {
		return n
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.ToLower(prefix)
}

func intEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		log.Fatalf("%s %q must be a positive integer", key, v)
	}
	return n
}

// guard enforces read-only access and, when credentials are configured,
// HTTP basic auth.
func guard(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeMethods[r.Method] {
			http.Error(w, "read-only filesystem", http.StatusForbidden)
			return
		}
		if user != "" && pass != "" {
			u, p, ok := r.BasicAuth()
			userOK := subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1
			passOK := subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
			if !ok || !userOK || !passOK {
				w.Header().Set("WWW-Authenticate", `Basic realm="flexdav"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
