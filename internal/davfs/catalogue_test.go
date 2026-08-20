package davfs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phiivas/flexdav/internal/plex"
)

// --- a small two-server fake ----------------------------------------

type fakeSection struct {
	key   string
	title string
	typ   string
	items []map[string]any
}

type fakeServer struct {
	sections []fakeSection
	srv      *httptest.Server
	// down, when set, makes every request fail, standing in for the
	// outages one of these providers has several times a day.
	down bool
	// deadSections fail while the rest of the server answers, which is
	// what an outage part way through a build actually looks like: the
	// section list came back, some sections were listed, and then the
	// server stopped answering for the rest.
	deadSections map[string]bool
	// children answers /library/metadata/<key>/children, so a test can
	// descend into a show. Seasons and episodes are never in the
	// catalogue; they are fetched live from whichever server won the
	// show, which is exactly what makes an outage able to close a
	// directory rather than merely a file.
	children map[string][]map[string]any
}

// film builds a movie carrying an external id, which is what makes it
// recognisable on another server.
func film(ratingKey, title string, year int, tmdb string, size int) map[string]any {
	m := map[string]any{
		"ratingKey": ratingKey,
		"key":       "/library/metadata/" + ratingKey,
		"type":      "movie",
		"title":     title,
		"year":      year,
		"updatedAt": 1700000000,
		"Media": []map[string]any{{
			"Part": []map[string]any{{
				// The size travels in the path so the fake can answer
				// with a body of exactly the length it promised; a short
				// body reads as a truncated transfer, not as a test.
				"key":       "/library/parts/" + ratingKey + "/" + strconv.Itoa(size) + "/file.mkv",
				"file":      "/data/" + ratingKey + " - " + title + ".mkv",
				"size":      size,
				"container": "mkv",
			}},
		}},
	}
	if tmdb != "" {
		m["Guid"] = []map[string]any{{"id": "tmdb://" + tmdb}}
	}
	return m
}

// show builds a series, which carries no file of its own.
func show(ratingKey, title, tmdb string) map[string]any {
	m := map[string]any{
		"ratingKey": ratingKey,
		"key":       "/library/metadata/" + ratingKey,
		"type":      "show",
		"title":     title,
		"updatedAt": 1700000000,
	}
	if tmdb != "" {
		m["Guid"] = []map[string]any{{"id": "tmdb://" + tmdb}}
	}
	return m
}

func newFakeServer(t *testing.T, sections ...fakeSection) *fakeServer {
	t.Helper()
	f := &fakeServer{sections: sections}
	mux := http.NewServeMux()

	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	guard := func(w http.ResponseWriter) bool {
		if f.down {
			http.Error(w, "upstream down", http.StatusBadGateway)
			return false
		}
		return true
	}

	mux.HandleFunc("/library/sections", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		dirs := make([]map[string]any, 0, len(f.sections))
		for _, s := range f.sections {
			dirs = append(dirs, map[string]any{"key": s.key, "title": s.title, "type": s.typ})
		}
		write(w, map[string]any{"MediaContainer": map[string]any{"Directory": dirs}})
	})

	for _, s := range f.sections {
		key := s.key
		mux.HandleFunc("/library/sections/"+key+"/all", func(w http.ResponseWriter, r *http.Request) {
			if !guard(w) {
				return
			}
			if f.deadSections[key] {
				http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
				return
			}
			// Looked up per request rather than captured, so a test can
			// add a title between two builds and have the server
			// actually serve it.
			items := f.itemsFor(key)
			start, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
			if start >= len(items) {
				write(w, map[string]any{"MediaContainer": map[string]any{"Metadata": []any{}}})
				return
			}
			write(w, map[string]any{"MediaContainer": map[string]any{"Metadata": items[start:]}})
		})
	}

	// Media reads answer with the rating key repeated, so a test can
	// tell which server actually served the bytes.
	mux.HandleFunc("/library/parts/", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		// path is /library/parts/<ratingKey>/<size>/file.mkv
		fields := strings.Split(r.URL.Path, "/")
		if len(fields) < 5 {
			http.Error(w, "bad part path", http.StatusBadRequest)
			return
		}
		ratingKey := fields[3]
		size, err := strconv.Atoi(fields[4])
		if err != nil {
			http.Error(w, "bad size", http.StatusBadRequest)
			return
		}
		full := make([]byte, size)
		for i := range full {
			full[i] = ratingKey[i%len(ratingKey)]
		}

		start, end := 0, size-1
		if rh := r.Header.Get("Range"); strings.HasPrefix(rh, "bytes=") {
			lo, hi, _ := strings.Cut(strings.TrimPrefix(rh, "bytes="), "-")
			if v, err := strconv.Atoi(lo); err == nil {
				start = v
			}
			if v, err := strconv.Atoi(hi); err == nil && v < end {
				end = v
			}
		}
		if start > end || start >= size {
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		chunk := full[start : end+1]
		w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(chunk)
	})

	mux.HandleFunc("/library/metadata/", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w) {
			return
		}
		// path is /library/metadata/<ratingKey>/children
		fields := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(fields) < 4 || fields[3] != "children" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		kids, ok := f.children[fields[2]]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		write(w, map[string]any{"MediaContainer": map[string]any{"Metadata": kids}})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// itemsFor returns a section's current items by key.
func (f *fakeServer) itemsFor(key string) []map[string]any {
	for _, s := range f.sections {
		if s.key == key {
			return s.items
		}
	}
	return nil
}

// season builds one season of a series. Like a show it holds no file.
func season(ratingKey, title string) map[string]any {
	return map[string]any{
		"ratingKey": ratingKey,
		"key":       "/library/metadata/" + ratingKey,
		"type":      "season",
		"title":     title,
		"updatedAt": 1700000000,
	}
}

func mergedFS(t *testing.T, servers ...*fakeServer) *FS {
	t.Helper()
	sources := make([]*Source, 0, len(servers))
	for i, s := range servers {
		sources = append(sources, &Source{
			Name:     "s" + strconv.Itoa(i),
			Client:   plex.NewClient(s.srv.URL, "tok"),
			Streams:  1,
			MaxChunk: defaultMaxChunk,
		})
	}
	fs := NewMulti(sources, Options{CacheTTL: time.Minute})
	// A partial catalogue is a published, usable one: it means a server
	// was unreachable, not that the build failed.
	if err := fs.Build(context.Background()); err != nil && !errors.Is(err, ErrPartialCatalogue) {
		t.Fatalf("Build: %v", err)
	}
	return fs
}

func names(t *testing.T, fs *FS, dir string) []string {
	t.Helper()
	return readdirNames(t, fs, dir)
}

// --- tests ----------------------------------------------------------

// The whole point: a film on both servers is indexed once. Measured
// against the real pair, 76,068 films sit on both, and letting the local
// Plex scan each of those twice is the single most expensive mistake
// available here.
func TestSameFilmOnBothServersAppearsOnce(t *testing.T) {
	a := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("a1", "The Matrix", 1999, "603", 1000),
		film("a2", "Alien", 1979, "348", 1000),
	}})
	b := newFakeServer(t, fakeSection{key: "9", title: "Movies", typ: "movie", items: []map[string]any{
		film("b1", "Matrix, The", 1999, "603", 5000), // same film, different name
		film("b2", "Solaris", 1972, "593", 1000),
	}})

	got := names(t, mergedFS(t, a, b), "/Movies")
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (two from the first server, one unique to the second): %v", len(got), got)
	}
	if !contains(got, "a1 - The Matrix.mkv") {
		t.Errorf("the preferred server's copy is missing: %v", got)
	}
	if contains(got, "b1 - Matrix, The.mkv") {
		t.Errorf("the duplicate from the backup server survived: %v", got)
	}
	if !contains(got, "b2 - Solaris.mkv") {
		t.Errorf("a film only the backup has went missing: %v", got)
	}
}

// The priority decision, made explicitly: the primary server wins even
// when the backup holds a bigger file. Its copies measured larger 51.7%
// of the time against 5.8%, and a stable choice is worth more than
// chasing the occasional better copy.
func TestPreferredServerWinsEvenWhenSmaller(t *testing.T) {
	a := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("small", "Dune", 2021, "438631", 1000),
	}})
	b := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("huge", "Dune", 2021, "438631", 90000),
	}})

	got := names(t, mergedFS(t, a, b), "/Movies")
	if len(got) != 1 || got[0] != "small - Dune.mkv" {
		t.Errorf("expected the preferred server's copy, got %v", got)
	}
}

// Within one server nothing is collapsed. A server's section layout is
// its owner's curation, and the sections that exist precisely to hold a
// different copy of the same film must survive.
//
// This was learned the expensive way. An earlier version deduplicated
// inside each server too, and against the real library "Movies - Kids"
// fell from 1,636 titles to 26, "Movies - 3D" from 900 to 66 and
// "Movies - 4K-dv" from 5,601 to 1,078: the ordinary copy in "Movies" is
// usually the bigger file, so the 3D and Dolby Vision versions lost to
// it. A 3D library holding 66 films is not tidier, it is broken.
func TestWithinOneServerNothingIsCollapsed(t *testing.T) {
	a := newFakeServer(t,
		fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
			film("flat", "Dune", 2021, "438631", 90000),
		}},
		fakeSection{key: "2", title: "Movies - 3D", typ: "movie", items: []map[string]any{
			film("3d", "Dune", 2021, "438631", 1000),
		}},
	)
	fs := mergedFS(t, a)

	if got := names(t, fs, "/Movies"); len(got) != 1 || got[0] != "flat - Dune.mkv" {
		t.Errorf("Movies holds %v, want its own copy", got)
	}
	if got := names(t, fs, "/Movies - 3D"); len(got) != 1 || got[0] != "3d - Dune.mkv" {
		t.Errorf("Movies - 3D holds %v, want the 3D copy kept", got)
	}
}

// A title the preferred server keeps in several sections blocks the
// other server's copy from every one of them, not just the first.
func TestABackupCopyLosesToEveryCopyOnThePreferredServer(t *testing.T) {
	a := newFakeServer(t,
		fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
			film("flat", "Dune", 2021, "438631", 1000),
		}},
		fakeSection{key: "2", title: "Movies - 3D", typ: "movie", items: []map[string]any{
			film("3d", "Dune", 2021, "438631", 1000),
		}},
	)
	b := newFakeServer(t,
		fakeSection{key: "1", title: "Movies - Remux", typ: "movie", items: []map[string]any{
			film("remux", "Dune", 2021, "438631", 90000),
		}},
	)
	fs := mergedFS(t, a, b)

	if got := names(t, fs, "/Movies - Remux"); len(got) != 0 {
		t.Errorf("the backup's duplicate survived: %v", got)
	}
	if got := names(t, fs, "/Movies"); len(got) != 1 {
		t.Errorf("Movies holds %v, want its own copy", got)
	}
	if got := names(t, fs, "/Movies - 3D"); len(got) != 1 {
		t.Errorf("Movies - 3D holds %v, want its own copy", got)
	}
}

// Sections from both servers appear, and the ones sharing a name become
// a single directory rather than "Movies" and "Movies (2)".
func TestSectionsAreMergedByName(t *testing.T) {
	a := newFakeServer(t,
		fakeSection{key: "1", title: "Movies", typ: "movie"},
		fakeSection{key: "2", title: "Movies - Anime", typ: "movie"},
	)
	b := newFakeServer(t,
		fakeSection{key: "1", title: "Movies", typ: "movie"},
		fakeSection{key: "3", title: "Movies - Remux", typ: "movie"},
	)

	got := names(t, mergedFS(t, a, b), "/")
	want := []string{"Movies", "Movies - Anime", "Movies - Remux"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("section %q missing from %v", w, got)
		}
	}
}

// Two different works can share a title, and about 1% of records carry
// no external id at all. Merging those on title alone would delete one
// of them, which is far worse than showing a duplicate.
func TestUnidentifiedTitlesAreNeverMerged(t *testing.T) {
	a := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("x1", "Untitled", 0, "", 1000),
		film("x2", "Untitled", 0, "", 1000),
	}})
	b := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("y1", "Untitled", 0, "", 1000),
	}})

	got := names(t, mergedFS(t, a, b), "/Movies")
	if len(got) != 3 {
		t.Errorf("got %d entries, want 3; unidentified titles must never be collapsed: %v", len(got), got)
	}
}

// TMDb numbers films and television separately, so the same small
// integer identifies an unrelated film and series at once. Merging on
// the number alone lost roughly 24,000 shows on the first real build.
func TestFilmsAndShowsWithTheSameIDStayApart(t *testing.T) {
	a := newFakeServer(t,
		fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
			film("m", "Some Film", 1999, "603", 1000),
		}},
		fakeSection{key: "2", title: "TV Shows", typ: "show", items: []map[string]any{
			show("s", "Some Series", "603"),
		}},
	)
	fs := mergedFS(t, a)

	if got := names(t, fs, "/Movies"); len(got) != 1 {
		t.Errorf("Movies holds %v, want the one film", got)
	}
	if got := names(t, fs, "/TV Shows"); len(got) != 1 {
		t.Errorf("TV Shows holds %v, want the one series", got)
	}
}

// A film that only the backup holds must be read from the backup. With
// one client per server and different tuning for each, reading it from
// the wrong one produces a tree that browses but will not open.
func TestFileIsReadFromItsOwnServer(t *testing.T) {
	a := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("a1", "Alien", 1979, "348", 1000),
	}})
	b := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("b1", "Solaris", 1972, "593", 1000),
	}})
	fs := mergedFS(t, a, b)

	fh, err := fs.OpenFile(context.Background(), "/Movies/b1 - Solaris.mkv", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fh.Close()
	body, err := io.ReadAll(fh)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// The fake fills the body with its own rating key, so this proves
	// which server was asked.
	if len(body) != 1000 {
		t.Fatalf("got %d bytes, want 1000", len(body))
	}
	if string(body[:2]) != "b1" {
		t.Errorf("served by the wrong server: body starts %q", body[:2])
	}
}

// One provider drops out for ten to twenty-five minutes at a time. When
// that happens the other server's library must still be there: an empty
// tree is what a scanner reads as "everything was deleted".
func TestOneServerDownStillExposesTheOther(t *testing.T) {
	a := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("a1", "Alien", 1979, "348", 1000),
	}})
	b := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("b1", "Solaris", 1972, "593", 1000),
	}})
	a.down = true

	fs := mergedFS(t, a, b)
	got := names(t, fs, "/Movies")
	if len(got) != 1 || got[0] != "b1 - Solaris.mkv" {
		t.Errorf("expected the surviving server's library, got %v", got)
	}

	// It must also say it is incomplete. Publishing a library missing a
	// whole server without a word looks to the scanner above like every
	// one of that server's files was deleted, so the caller has to know
	// to keep rebuilding until the server answers.
	if err := fs.Build(context.Background()); !errors.Is(err, ErrPartialCatalogue) {
		t.Errorf("Build reported %v, want ErrPartialCatalogue", err)
	}
}

// Every server being unreachable is a different case: there is nothing
// to serve, and saying so beats publishing an empty library.
func TestAllServersDownIsAnError(t *testing.T) {
	a := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie"})
	a.down = true

	fs := NewMulti([]*Source{{Name: "a", Client: plex.NewClient(a.srv.URL, "tok")}},
		Options{CacheTTL: time.Minute})
	if err := fs.Build(context.Background()); err == nil {
		t.Error("building with every server down should fail rather than expose nothing")
	}
}

// A title the backup also holds is kept as a fallback, but only when the
// file is byte-for-byte the same length. The size is published when the
// directory is listed and the client remembers it, so a differently
// sized stand-in breaks seeking and cuts the ending off.
func TestOnlyIdenticalCopiesBecomeFallbacks(t *testing.T) {
	a := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("same", "Dune", 2021, "438631", 5000),
		film("diff", "Alien", 1979, "348", 5000),
	}})
	b := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("same-copy", "Dune", 2021, "438631", 5000), // identical length
		film("diff-copy", "Alien", 1979, "348", 999000), // a different encoding
	}})
	fs := mergedFS(t, a, b)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/Movies/same - Dune.mkv", 1},
		{"/Movies/diff - Alien.mkv", 0},
	} {
		n, err := fs.resolve(context.Background(), tc.path)
		if err != nil {
			t.Fatalf("resolve %s: %v", tc.path, err)
		}
		if got := len(n.alts); got != tc.want {
			t.Errorf("%s has %d fallbacks, want %d", tc.path, got, tc.want)
		}
	}
}

// A film the preferred server keeps in two sections wants the fallback
// attached to both, not just whichever was seen first.
func TestFallbacksReachEveryCopyOnThePreferredServer(t *testing.T) {
	a := newFakeServer(t,
		fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
			film("flat", "Dune", 2021, "438631", 5000),
		}},
		fakeSection{key: "2", title: "Movies - Kids", typ: "movie", items: []map[string]any{
			film("kids", "Dune", 2021, "438631", 5000),
		}},
	)
	b := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("spare", "Dune", 2021, "438631", 5000),
	}})
	fs := mergedFS(t, a, b)

	for _, p := range []string{"/Movies/flat - Dune.mkv", "/Movies - Kids/kids - Dune.mkv"} {
		n, err := fs.resolve(context.Background(), p)
		if err != nil {
			t.Fatalf("resolve %s: %v", p, err)
		}
		if len(n.alts) != 1 {
			t.Errorf("%s has %d fallbacks, want 1", p, len(n.alts))
		}
	}
}
