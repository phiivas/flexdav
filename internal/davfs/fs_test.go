package davfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/webdav"

	"github.com/phiivas/flexdav/internal/plex"
)

// --- fake Plex server -----------------------------------------------

// partBody is deterministic content for a media part: byte i is
// i%251. The prime keeps the pattern from aligning with power-of-two
// buffer sizes, so an off-by-buffer bug shows up as a mismatch.
func partBody(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

const moviePartSize = 300000

type fakePlex struct {
	// truncations, while positive, makes each media request drop the
	// connection halfway through, simulating the flaky CDN.
	truncations  atomic.Int32
	partGETs     atomic.Int32
	childrenGETs atomic.Int32
	// listGETs counts first pages fetched per section, which is how
	// tests tell whether a section was genuinely re-listed.
	listGETs sync.Map // section key -> *atomic.Int32
	server   *httptest.Server
}

// countList records one listing of a section, counting only the first
// page so that pagination does not inflate the figure.
func (f *fakePlex) countList(key string, r *http.Request) {
	if r.URL.Query().Get("X-Plex-Container-Start") != "0" {
		return
	}
	v, _ := f.listGETs.LoadOrStore(key, new(atomic.Int32))
	v.(*atomic.Int32).Add(1)
}

func (f *fakePlex) listCount(key string) int32 {
	if v, ok := f.listGETs.Load(key); ok {
		return v.(*atomic.Int32).Load()
	}
	return 0
}

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// movieItem builds one movie's JSON. file may be empty to exercise the
// Title+container naming fallback.
func movieItem(ratingKey, title, file string, size int) map[string]any {
	return map[string]any{
		"ratingKey": ratingKey,
		"key":       "/library/metadata/" + ratingKey,
		"type":      "movie",
		"title":     title,
		"updatedAt": 1700000000,
		"Media": []map[string]any{{
			"Part": []map[string]any{{
				"key":       "/library/parts/" + ratingKey + "/1/file.mkv",
				"file":      file,
				"size":      size,
				"container": "mkv",
			}},
		}},
	}
}

func branchItem(ratingKey, typ, title string) map[string]any {
	return map[string]any{
		"ratingKey": ratingKey,
		"key":       "/library/metadata/" + ratingKey,
		"type":      typ,
		"title":     title,
		"updatedAt": 1700000000,
	}
}

func (f *fakePlex) movies() []map[string]any {
	out := []map[string]any{
		movieItem("m1", "The Matrix", "/data/movies/The Matrix (1999) Bluray-1080p.mkv", moviePartSize),
		movieItem("m2", "No File Attribute", "", moviePartSize),
		movieItem("m3", "Duplicate Title", "", moviePartSize),
		movieItem("m4", "Duplicate Title", "", moviePartSize),
		movieItem("m5", "Face/Off", "", moviePartSize),
	}
	// Pad past one page so pagination is genuinely exercised.
	for i := 0; i < 1200; i++ {
		out = append(out, movieItem(
			fmt.Sprintf("pad%d", i),
			fmt.Sprintf("Padding Movie %04d", i),
			"",
			1000,
		))
	}
	return out
}

func (f *fakePlex) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/identity", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, map[string]any{"MediaContainer": map[string]any{"machineIdentifier": "fake"}})
	})

	mux.HandleFunc("/library/sections", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, map[string]any{"MediaContainer": map[string]any{
			"Directory": []map[string]any{
				{"key": "1", "type": "movie", "title": "Movies - 4K"},
				{"key": "2", "type": "show", "title": "TV Shows - 4K"},
				{"key": "3", "type": "movie", "title": "Hidden Library"},
			},
		}})
	})

	page := func(w http.ResponseWriter, r *http.Request, all []map[string]any) {
		start, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
		size, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Size"))
		if size <= 0 {
			size = len(all)
		}
		if start > len(all) {
			start = len(all)
		}
		end := start + size
		if end > len(all) {
			end = len(all)
		}
		jsonResp(w, map[string]any{"MediaContainer": map[string]any{
			"size":     end - start,
			"Metadata": all[start:end],
			// totalSize deliberately omitted: several real Plex
			// endpoints leave it out, and pagination must not depend
			// on it.
		}})
	}

	mux.HandleFunc("/library/sections/1/all", func(w http.ResponseWriter, r *http.Request) {
		f.countList("1", r)
		page(w, r, f.movies())
	})
	mux.HandleFunc("/library/sections/2/all", func(w http.ResponseWriter, r *http.Request) {
		f.countList("2", r)
		page(w, r, []map[string]any{branchItem("show1", "show", "Yellowstone")})
	})
	mux.HandleFunc("/library/sections/3/all", func(w http.ResponseWriter, r *http.Request) {
		f.countList("3", r)
		page(w, r, []map[string]any{movieItem("h1", "Hidden Movie", "", 10)})
	})

	mux.HandleFunc("/library/metadata/show1/children", func(w http.ResponseWriter, r *http.Request) {
		f.childrenGETs.Add(1)
		page(w, r, []map[string]any{branchItem("season3", "season", "Season 3")})
	})
	mux.HandleFunc("/library/metadata/season3/children", func(w http.ResponseWriter, r *http.Request) {
		f.childrenGETs.Add(1)
		page(w, r, []map[string]any{
			movieItem("ep7", "The Beating", "/data/tv/Yellowstone - S03E07 [WEBDL-2160p].mkv", moviePartSize),
		})
	})

	mux.HandleFunc("/library/parts/", func(w http.ResponseWriter, r *http.Request) {
		f.partGETs.Add(1)
		if r.URL.Query().Get("X-Plex-Token") == "" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		full := partBody(moviePartSize)

		// Handles both open-ended ("bytes=N-") and closed ("bytes=N-M")
		// forms; the chunked reader issues the latter.
		start, end := int64(0), int64(len(full)-1)
		if rh := r.Header.Get("Range"); strings.HasPrefix(rh, "bytes=") {
			lo, hi, _ := strings.Cut(strings.TrimPrefix(rh, "bytes="), "-")
			v, err := strconv.ParseInt(lo, 10, 64)
			if err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			start = v
			if hi != "" {
				v, err := strconv.ParseInt(hi, 10, 64)
				if err != nil {
					http.Error(w, "bad range", http.StatusBadRequest)
					return
				}
				if v < end {
					end = v
				}
			}
		}
		if start >= int64(len(full)) || end < start {
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		chunk := full[start : end+1]

		w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, end, len(full)))
		w.WriteHeader(http.StatusPartialContent)

		if f.truncations.Load() > 0 {
			f.truncations.Add(-1)
			// Promise the full length, deliver half, kill the
			// connection: exactly how the real CDN fails.
			_, _ = w.Write(chunk[:len(chunk)/2])
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(chunk)
	})

	return mux
}

func newFake(t *testing.T) *fakePlex {
	t.Helper()
	f := &fakePlex{}
	f.server = httptest.NewServer(f.handler())
	t.Cleanup(f.server.Close)
	return f
}

func newFS(t *testing.T, f *fakePlex, sections ...string) *FS {
	t.Helper()
	fs := New(plex.NewClient(f.server.URL, "test-token"), Options{
		CacheTTL: time.Minute,
		Sections: sections,
		Streams:  defaultStreams,
		MaxChunk: defaultMaxChunk,
	})
	// Nothing can be listed before the catalogue exists: which titles
	// belong in a section is a question about every server at once.
	if err := fs.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return fs
}

func readdirNames(t *testing.T, fs *FS, dir string) []string {
	t.Helper()
	fh, err := fs.OpenFile(context.Background(), dir, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", dir, err)
	}
	defer fh.Close()
	infos, err := fh.Readdir(-1)
	if err != nil {
		t.Fatalf("Readdir(%q): %v", dir, err)
	}
	names := make([]string, 0, len(infos))
	for _, fi := range infos {
		names = append(names, fi.Name())
	}
	return names
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// --- tests ----------------------------------------------------------

// The root listing was returning nothing at all in the first draft,
// which makes the whole mount look empty.
func TestRootListsSections(t *testing.T) {
	fs := newFS(t, newFake(t))
	names := readdirNames(t, fs, "/")
	for _, want := range []string{"Movies - 4K", "TV Shows - 4K", "Hidden Library"} {
		if !contains(names, want) {
			t.Errorf("root listing missing %q; got %v", want, names)
		}
	}
}

func TestSectionFilterHidesOthers(t *testing.T) {
	fs := newFS(t, newFake(t), "Movies - 4K")
	names := readdirNames(t, fs, "/")
	if len(names) != 1 || names[0] != "Movies - 4K" {
		t.Fatalf("expected only the allowed section, got %v", names)
	}
	if _, err := fs.Stat(context.Background(), "/Hidden Library"); !os.IsNotExist(err) {
		t.Errorf("filtered section should not resolve, got err=%v", err)
	}
}

// Listing and lookup have to agree on names: the first draft listed
// files as "Title.mkv" but matched lookups against "Title", so every
// file was visible and none could be opened.
func TestListedNamesAreOpenable(t *testing.T) {
	fs := newFS(t, newFake(t))
	names := readdirNames(t, fs, "/Movies - 4K")
	for _, n := range names {
		if _, err := fs.Stat(context.Background(), "/Movies - 4K/"+n); err != nil {
			t.Fatalf("listed %q but Stat failed: %v", n, err)
		}
	}
}

func TestNamingPrefersServerFilename(t *testing.T) {
	fs := newFS(t, newFake(t))
	names := readdirNames(t, fs, "/Movies - 4K")

	// Real release filename wins when Plex exposes Part.file, since
	// that is what Sonarr and Radarr can parse.
	if !contains(names, "The Matrix (1999) Bluray-1080p.mkv") {
		t.Errorf("expected server-side filename in listing; got sample %v", names[:5])
	}
	// Fallback when it does not.
	if !contains(names, "No File Attribute.mkv") {
		t.Errorf("expected Title.container fallback; got sample %v", names[:5])
	}
}

func TestDuplicateTitlesBothResolve(t *testing.T) {
	fs := newFS(t, newFake(t))
	names := readdirNames(t, fs, "/Movies - 4K")
	if !contains(names, "Duplicate Title.mkv") || !contains(names, "Duplicate Title (2).mkv") {
		t.Fatalf("duplicates not disambiguated; got %v", names[:5])
	}
	for _, n := range []string{"Duplicate Title.mkv", "Duplicate Title (2).mkv"} {
		if _, err := fs.Stat(context.Background(), "/Movies - 4K/"+n); err != nil {
			t.Errorf("Stat(%q): %v", n, err)
		}
	}
}

// A title like "Face/Off" would otherwise invent a directory level.
func TestSlashInTitleIsSanitized(t *testing.T) {
	fs := newFS(t, newFake(t))
	names := readdirNames(t, fs, "/Movies - 4K")
	if !contains(names, "Face-Off.mkv") {
		t.Errorf("expected sanitized name Face-Off.mkv; got sample %v", names[:5])
	}
	if _, err := fs.Stat(context.Background(), "/Movies - 4K/Face-Off.mkv"); err != nil {
		t.Errorf("sanitized name should resolve: %v", err)
	}
}

// Pagination must be driven by short pages, not by totalSize, which
// the fake server omits exactly as some real endpoints do.
func TestPaginationReturnsEverything(t *testing.T) {
	fs := newFS(t, newFake(t))
	names := readdirNames(t, fs, "/Movies - 4K")
	if want := 1205; len(names) != want {
		t.Fatalf("got %d entries, want %d (pagination truncated the listing)", len(names), want)
	}
}

func TestShowSeasonEpisodeTraversal(t *testing.T) {
	fs := newFS(t, newFake(t))
	ctx := context.Background()

	if names := readdirNames(t, fs, "/TV Shows - 4K"); !contains(names, "Yellowstone") {
		t.Fatalf("show missing: %v", names)
	}
	if names := readdirNames(t, fs, "/TV Shows - 4K/Yellowstone"); !contains(names, "Season 3") {
		t.Fatalf("season missing: %v", names)
	}
	const ep = "/TV Shows - 4K/Yellowstone/Season 3/Yellowstone - S03E07 [WEBDL-2160p].mkv"
	fi, err := fs.Stat(ctx, ep)
	if err != nil {
		t.Fatalf("Stat(episode): %v", err)
	}
	if fi.IsDir() {
		t.Error("episode should be a file")
	}
	if fi.Size() != moviePartSize {
		t.Errorf("size = %d, want %d", fi.Size(), moviePartSize)
	}
}

// Stat on a directory must not pull its contents. A file browser stats
// every entry it lists, so eagerly loading each show's seasons turned
// listing a 1833-show section into 1833 serial API calls and hung the
// Unraid file manager outright.
func TestStatOnDirectoryDoesNotLoadChildren(t *testing.T) {
	f := newFake(t)
	fs := newFS(t, f)
	ctx := context.Background()

	// Listing the section itself is one call; stat-ing the show inside
	// it must add none.
	names := readdirNames(t, fs, "/TV Shows - 4K")
	if !contains(names, "Yellowstone") {
		t.Fatalf("show missing: %v", names)
	}
	before := f.childrenGETs.Load()

	for i := 0; i < 10; i++ {
		if _, err := fs.Stat(ctx, "/TV Shows - 4K/Yellowstone"); err != nil {
			t.Fatalf("Stat: %v", err)
		}
	}
	if got := f.childrenGETs.Load(); got != before {
		t.Errorf("stat-ing a directory triggered %d child fetch(es); it must be metadata-only", got-before)
	}

	// Actually reading it should, of course, fetch.
	if names := readdirNames(t, fs, "/TV Shows - 4K/Yellowstone"); !contains(names, "Season 3") {
		t.Fatalf("season missing after read: %v", names)
	}
	if f.childrenGETs.Load() == before {
		t.Error("reading the directory should have fetched its children")
	}
}

// Stat must be a lookup, not a rebuild. Clients stat every entry they
// list, so reconstructing the entry slice per stat is quadratic: the
// real 99,110-item "Movies" section pinned a core at 162% and never
// finished listing.
func TestStatDoesNotRebuildListing(t *testing.T) {
	f := newFake(t)
	fs := newFS(t, f)
	ctx := context.Background()

	names := readdirNames(t, fs, "/Movies - 4K")
	if len(names) < 100 {
		t.Fatalf("expected a large listing, got %d", len(names))
	}
	built := fs.listingsBuilt.Load()
	lists := f.listCount("1")

	for _, n := range names[:200] {
		if _, err := fs.Stat(ctx, "/Movies - 4K/"+n); err != nil {
			t.Fatalf("Stat(%q): %v", n, err)
		}
	}
	if got := fs.listingsBuilt.Load(); got != built {
		t.Errorf("200 stats rebuilt the listing %d time(s); it must be built once and indexed", got-built)
	}
	if got := f.listCount("1"); got != lists {
		t.Errorf("200 stats caused %d extra API listings", got-lists)
	}
}

// New episodes and titles should surface without waiting out the whole
// cache TTL, but only the sections Plex reports as changed may be
// re-listed: re-listing everything on a timer would mean minutes of
// work against the large libraries.
func TestOnlyChangedSectionsAreRefreshed(t *testing.T) {
	f := newFake(t)
	fs := newFS(t, f)
	ctx := context.Background()

	// Build listed everything once.
	if got := f.listCount("1"); got != 1 {
		t.Fatalf("build listed section 1 %d times, want 1", got)
	}
	before2 := f.listCount("2")

	// First sighting only records the stamp; an unchanged stamp after
	// that is a no-op.
	stamp := []plex.Directory{{Key: "1", Title: "Movies - 4K", UpdatedAt: 100}}
	fs.invalidateChanged(0, stamp)
	fs.invalidateChanged(0, stamp)
	if err := fs.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := f.listCount("1"); got != 1 {
		t.Errorf("unchanged section was re-listed (%d times total)", got)
	}

	// A bumped stamp must re-list that section, and only that one.
	fs.invalidateChanged(0, []plex.Directory{{Key: "1", Title: "Movies - 4K", UpdatedAt: 200}})
	if err := fs.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := f.listCount("1"); got != 2 {
		t.Errorf("changed section listed %d times, want 2", got)
	}
	if got := f.listCount("2"); got != before2 {
		t.Errorf("untouched section was re-listed %d time(s)", got-before2)
	}

	// The tree must still be intact afterwards.
	if names := readdirNames(t, fs, "/Movies - 4K"); len(names) < 100 {
		t.Errorf("after refresh the section holds %d entries", len(names))
	}
}

// A section whose stamp moved has to be re-listed, but everything else
// must be re-derived too: a film arriving in one section can take a
// title away from another section that did not itself change.
func TestRefreshRederivesTheWholeTree(t *testing.T) {
	f := newFake(t)
	fs := newFS(t, f)
	ctx := context.Background()

	before := len(readdirNames(t, fs, "/Movies - 4K"))
	fs.invalidateChanged(0, []plex.Directory{{Key: "1", Title: "Movies - 4K", UpdatedAt: 1}})
	fs.invalidateChanged(0, []plex.Directory{{Key: "1", Title: "Movies - 4K", UpdatedAt: 2}})
	if err := fs.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := len(readdirNames(t, fs, "/Movies - 4K")); got != before {
		t.Errorf("listing changed size across a refresh: %d then %d", before, got)
	}
}

func TestPathBeyondFileIsNotFound(t *testing.T) {
	fs := newFS(t, newFake(t))
	_, err := fs.Stat(context.Background(), "/Movies - 4K/No File Attribute.mkv/nested")
	if !os.IsNotExist(err) {
		t.Errorf("expected not-exist, got %v", err)
	}
}

func TestMutationsRejected(t *testing.T) {
	fs := newFS(t, newFake(t))
	ctx := context.Background()
	if err := fs.Mkdir(ctx, "/x", 0o755); err != ErrReadOnly {
		t.Errorf("Mkdir: %v", err)
	}
	if err := fs.RemoveAll(ctx, "/x"); err != ErrReadOnly {
		t.Errorf("RemoveAll: %v", err)
	}
	if err := fs.Rename(ctx, "/a", "/b"); err != ErrReadOnly {
		t.Errorf("Rename: %v", err)
	}
	if _, err := fs.OpenFile(ctx, "/Movies - 4K/x.mkv", os.O_WRONLY|os.O_CREATE, 0o644); err != ErrReadOnly {
		t.Errorf("OpenFile(write): %v", err)
	}
}

func TestReadWholeFile(t *testing.T) {
	fs := newFS(t, newFake(t))
	fh, err := fs.OpenFile(context.Background(), "/Movies - 4K/The Matrix (1999) Bluray-1080p.mkv", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fh.Close()

	got, err := io.ReadAll(fh)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := partBody(moviePartSize)
	if len(got) != len(want) {
		t.Fatalf("read %d bytes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("content differs at byte %d: got %d want %d", i, got[i], want[i])
		}
	}
}

// Seeking is what separates a mountable filesystem from a download
// tool: players and rclone jump around constantly.
func TestSeekReadsCorrectBytes(t *testing.T) {
	fs := newFS(t, newFake(t))
	fh, err := fs.OpenFile(context.Background(), "/Movies - 4K/The Matrix (1999) Bluray-1080p.mkv", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fh.Close()
	want := partBody(moviePartSize)

	for _, off := range []int64{0, 1, 12345, moviePartSize - 10, 5000, 250000} {
		if _, err := fh.Seek(off, io.SeekStart); err != nil {
			t.Fatalf("Seek(%d): %v", off, err)
		}
		buf := make([]byte, 8)
		if int(off)+len(buf) > len(want) {
			buf = buf[:len(want)-int(off)]
		}
		if _, err := io.ReadFull(fh, buf); err != nil {
			t.Fatalf("ReadFull at %d: %v", off, err)
		}
		for i := range buf {
			if buf[i] != want[int(off)+i] {
				t.Fatalf("at offset %d byte %d: got %d want %d", off, i, buf[i], want[int(off)+i])
			}
		}
	}

	// SeekEnd must land relative to the true size.
	pos, err := fh.Seek(-4, io.SeekEnd)
	if err != nil {
		t.Fatalf("SeekEnd: %v", err)
	}
	if pos != moviePartSize-4 {
		t.Errorf("SeekEnd gave %d, want %d", pos, moviePartSize-4)
	}
}

// A short forward hop should be absorbed by the open stream rather than
// costing a fresh request against a high-latency CDN.
func TestSmallForwardSeekReusesConnection(t *testing.T) {
	f := newFake(t)
	fs := newFS(t, f)
	fh, err := fs.OpenFile(context.Background(), "/Movies - 4K/The Matrix (1999) Bluray-1080p.mkv", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fh.Close()

	buf := make([]byte, 16)
	if _, err := io.ReadFull(fh, buf); err != nil {
		t.Fatalf("initial read: %v", err)
	}
	after := f.partGETs.Load()

	if _, err := fh.Seek(1024, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, err := io.ReadFull(fh, buf); err != nil {
		t.Fatalf("read after small seek: %v", err)
	}
	if got := f.partGETs.Load(); got != after {
		t.Errorf("small forward seek issued %d extra request(s); expected reuse", got-after)
	}

	want := partBody(moviePartSize)
	for i := range buf {
		if buf[i] != want[1024+i] {
			t.Fatalf("byte %d after small seek: got %d want %d", i, buf[i], want[1024+i])
		}
	}
}

// The headline behaviour: a connection dropped mid-file resumes from
// the byte reached instead of failing the whole read.
func TestResumesAfterUpstreamDropsConnection(t *testing.T) {
	f := newFake(t)
	f.truncations.Store(3) // fail the first three attempts partway
	fs := newFS(t, f)

	fh, err := fs.OpenFile(context.Background(), "/Movies - 4K/The Matrix (1999) Bluray-1080p.mkv", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fh.Close()

	got, err := io.ReadAll(fh)
	if err != nil {
		t.Fatalf("ReadAll through dropped connections: %v", err)
	}
	want := partBody(moviePartSize)
	if len(got) != len(want) {
		t.Fatalf("recovered %d bytes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("content differs at byte %d after resume: got %d want %d", i, got[i], want[i])
		}
	}
	if f.truncations.Load() != 0 {
		t.Errorf("expected all simulated failures to be consumed, %d left", f.truncations.Load())
	}
	if f.partGETs.Load() < 4 {
		t.Errorf("expected reconnects, only %d part requests made", f.partGETs.Load())
	}
}

// End to end through the actual WebDAV handler, which is how rclone
// will talk to this.
func TestWebDAVGetWithRange(t *testing.T) {
	f := newFake(t)
	dav := &webdav.Handler{
		FileSystem: newFS(t, f),
		LockSystem: webdav.NewMemLS(),
	}
	srv := httptest.NewServer(dav)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/Movies%20-%204K/The%20Matrix%20(1999)%20Bluray-1080p.mkv", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=1000-1063")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %s, want 206 (range unsupported means no seeking when mounted)", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != 64 {
		t.Fatalf("got %d bytes, want 64", len(body))
	}
	want := partBody(moviePartSize)
	for i := range body {
		if body[i] != want[1000+i] {
			t.Fatalf("range byte %d: got %d want %d", i, body[i], want[1000+i])
		}
	}
}

func TestWebDAVPropfindListsRoot(t *testing.T) {
	dav := &webdav.Handler{
		FileSystem: newFS(t, newFake(t)),
		LockSystem: webdav.NewMemLS(),
	}
	srv := httptest.NewServer(dav)
	defer srv.Close()

	req, err := http.NewRequest("PROPFIND", srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Depth", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PROPFIND: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %s, want 207", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Movies") {
		t.Errorf("PROPFIND body does not mention the movie section: %s", truncate(string(body), 400))
	}
}

// Listing a directory must stay metadata-only. webdav's default
// content-type detection opens every file and sniffs 512 bytes, which
// against a remote library means one CDN round trip per entry: a real
// section took over 150 seconds to list before fileInfo grew its own
// ContentType method.
func TestPropfindDoesNotFetchFileContent(t *testing.T) {
	f := newFake(t)
	dav := &webdav.Handler{
		FileSystem: newFS(t, f),
		LockSystem: webdav.NewMemLS(),
	}
	srv := httptest.NewServer(dav)
	defer srv.Close()

	req, err := http.NewRequest("PROPFIND", srv.URL+"/Movies%20-%204K/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Depth", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PROPFIND: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %s, want 207", resp.Status)
	}
	if n := f.partGETs.Load(); n != 0 {
		t.Errorf("PROPFIND fetched media %d time(s); listings must be metadata-only", n)
	}
	if !strings.Contains(string(body), "video/x-matroska") {
		t.Errorf("expected mkv content type in response: %s", truncate(string(body), 300))
	}
	// Sanity: the listing really did cover every entry.
	if got := strings.Count(string(body), "<D:href>"); got < 1200 {
		t.Errorf("only %d hrefs in listing, expected the full section", got)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
