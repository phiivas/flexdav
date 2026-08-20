package davfs

import (
	"context"
	"testing"
	"time"

	"github.com/phiivas/flexdav/internal/plex"
)

func oneFilmServer(t *testing.T) *fakeServer {
	t.Helper()
	return newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("a1", "The Matrix", 1999, "603", 1000),
		film("a2", "Arrival", 2016, "329865", 2000),
	}})
}

func bareFS(t *testing.T, servers ...*fakeServer) *FS {
	t.Helper()
	sources := make([]*Source, 0, len(servers))
	for i, s := range servers {
		sources = append(sources, &Source{
			Name:     "s" + string(rune('0'+i)),
			Client:   plex.NewClient(s.srv.URL, "tok"),
			Streams:  1,
			MaxChunk: defaultMaxChunk,
		})
	}
	return NewMulti(sources, Options{CacheTTL: time.Minute})
}

// The catalogue a scanner is reading must never shrink because a server
// went away. To Plex a shorter listing is not an outage, it is a
// deletion, and it will act on it.
func TestAnOutageDoesNotReplaceAGoodCatalogue(t *testing.T) {
	a := oneFilmServer(t)
	fs := bareFS(t, a)

	if err := fs.Build(context.Background()); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	before := titleCount(fs.cat.get())
	if before != 2 {
		t.Fatalf("first build holds %d titles, want 2", before)
	}

	a.down = true
	// The error itself varies with how far the build got: outright
	// failure, ErrPartialCatalogue, or ErrStaleCatalogue when the
	// previous attempt's listings covered everything. What must hold
	// either way is that the served catalogue does not shrink.
	if err := fs.Build(context.Background()); err == nil {
		t.Fatal("Build during the outage reported success")
	}

	if got := titleCount(fs.cat.get()); got != before {
		t.Errorf("catalogue now holds %d titles, want the %d it had before the outage", got, before)
	}
	if got := names(t, fs, "/Movies"); len(got) != 2 {
		t.Errorf("Movies lists %v during the outage, want both films still there", got)
	}
}

// With nothing built yet and every server unreachable, there is no
// honest empty answer to give. Callers wait instead, because "the
// library is unknown" and "the library is empty" are not the same thing
// to whatever is scanning it.
func TestAnEmptyFirstBuildIsNotPublished(t *testing.T) {
	a := oneFilmServer(t)
	a.down = true
	fs := bareFS(t, a)

	err := fs.Build(context.Background())
	if err == nil {
		t.Fatal("Build with the only server down returned no error")
	}
	if c := fs.cat.get(); c != nil {
		t.Errorf("an empty catalogue was published: %d sections", len(c.sections))
	}

	// And a reader gets a timeout rather than an empty directory.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := fs.sectionListing(ctx, "Movies"); err == nil {
		t.Error("reading during the blackout returned a listing, want an error")
	}
}

// Once a server comes back the build must take effect again, or the
// guard above would freeze the catalogue at its first version forever.
func TestTheCatalogueUpdatesAgainOnceTheServerReturns(t *testing.T) {
	a := oneFilmServer(t)
	fs := bareFS(t, a)
	if err := fs.Build(context.Background()); err != nil {
		t.Fatalf("first Build: %v", err)
	}

	a.down = true
	_ = fs.Build(context.Background())

	a.down = false
	a.sections[0].items = append(a.sections[0].items, film("a3", "Dune", 2021, "438631", 3000))
	if err := fs.Build(context.Background()); err != nil {
		t.Fatalf("Build after recovery: %v", err)
	}
	if got := titleCount(fs.cat.get()); got != 3 {
		t.Errorf("catalogue holds %d titles after recovery, want 3", got)
	}
}
