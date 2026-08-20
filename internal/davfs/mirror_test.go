package davfs

import (
	"context"
	"testing"
)

func withMirror(t *testing.T) {
	t.Helper()
	prev := mirrorPrimary
	mirrorPrimary = true
	t.Cleanup(func() { mirrorPrimary = prev })
}

// twoServersOneShow builds a series both servers know, with seasons on
// each, so the fallback has something to answer with.
func twoServersOneShow(t *testing.T) (*fakeServer, *fakeServer, *FS) {
	t.Helper()
	a := newFakeServer(t, fakeSection{key: "1", title: "TV Shows", typ: "show", items: []map[string]any{
		show("a1", "Doug", "2225"),
	}})
	a.children = map[string][]map[string]any{
		"a1": {season("as1", "Season 1"), season("as2", "Season 2")},
	}
	b := newFakeServer(t, fakeSection{key: "1", title: "TV Shows", typ: "show", items: []map[string]any{
		show("b1", "Doug", "2225"),
	}})
	b.children = map[string][]map[string]any{
		"b1": {season("bs1", "Season 1"), season("bs2", "Season 2"), season("bs3", "Season 3")},
	}
	return a, b, mergedFS(t, a, b)
}

// A series folder must keep opening when the server that won it stops
// answering. This is not the same stake as a film that will not play: a
// directory that never returns blocks the scanner, and a blocked scanner
// holds the whole Plex database, not just its own section.
func TestShowListsFromTheOtherServerWhenItsOwnIsDown(t *testing.T) {
	a, _, fs := twoServersOneShow(t)

	if got := names(t, fs, "/TV Shows/Doug"); len(got) != 2 {
		t.Fatalf("seasons before the outage: %v, want 2", got)
	}

	a.down = true
	fs.cache = map[string]cacheEntry{} // the point is the fetch, not the cache

	got := names(t, fs, "/TV Shows/Doug")
	if len(got) != 3 {
		t.Errorf("seasons during the outage: %v, want the 3 the other server holds", got)
	}
}

// With no second copy the failure has to surface rather than hang, so
// the caller can give up instead of holding locks.
func TestShowWithNoOtherCopyFailsWhenItsServerIsDown(t *testing.T) {
	a := newFakeServer(t, fakeSection{key: "1", title: "TV Shows", typ: "show", items: []map[string]any{
		show("a1", "Only Here", "999"),
	}})
	a.children = map[string][]map[string]any{"a1": {season("as1", "Season 1")}}
	fs := mergedFS(t, a)

	a.down = true
	fs.cache = map[string]cacheEntry{}

	// Opening the directory is deliberately cheap and must not fetch
	// anything; the failure has to surface when it is actually read.
	h, err := fs.OpenFile(context.Background(), "/TV Shows/Only Here", 0, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()

	if _, err := h.Readdir(-1); err == nil {
		t.Error("expected an error reading the directory with the only server gone")
	}
}

// Mirrored, the second server adds nothing of its own. Against the real
// pair that is 17,824 films and 6,627 series left out on purpose: the
// tree is the primary's library exactly.
func TestMirrorAddsNothingFromTheFallbackServer(t *testing.T) {
	withMirror(t)

	a := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("a1", "Shared", 1999, "603", 1000),
	}})
	b := newFakeServer(t,
		fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
			film("b1", "Shared", 1999, "603", 1000),
			film("b2", "Only On The Backup", 2001, "777", 2000),
		}},
		fakeSection{key: "2", title: "Sports - PPV", typ: "movie", items: []map[string]any{
			film("b3", "A Fight", 2020, "888", 3000),
		}},
	)
	fs := mergedFS(t, a, b)

	got := names(t, fs, "/Movies")
	if len(got) != 1 {
		t.Errorf("Movies holds %v, want only the primary's one film", got)
	}

	// A section only the fallback has must not appear at all, rather than
	// appear empty.
	for _, s := range names(t, fs, "/") {
		if s == "Sports - PPV" {
			t.Error("a section only the fallback server has was exposed")
		}
	}
}

// Leaving titles out must not cost the fallback its real job. The same
// film on both servers still gets its stand-in copy.
func TestMirrorStillAttachesFallbacks(t *testing.T) {
	withMirror(t)

	a := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("a1", "Shared", 1999, "603", 1000),
	}})
	b := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("b1", "Shared", 1999, "603", 1000),
	}})
	fs := mergedFS(t, a, b)

	n, err := fs.OpenFile(context.Background(), "/Movies/a1 - Shared.mkv", 0, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer n.Close()

	f, ok := n.(*fileHandle)
	if !ok {
		t.Fatalf("opened %T, want a file", n)
	}
	if len(f.reader.srcs) != 2 {
		t.Errorf("file has %d sources, want the primary plus its fallback", len(f.reader.srcs))
	}
}

// And a show mirrored from the primary still carries the other server as
// somewhere to list from.
func TestMirrorStillAttachesShowFallbacks(t *testing.T) {
	withMirror(t)

	a, _, fs := twoServersOneShow(t)
	a.down = true
	fs.cache = map[string]cacheEntry{}

	if got := names(t, fs, "/TV Shows/Doug"); len(got) != 3 {
		t.Errorf("seasons during the outage: %v, want the 3 from the fallback", got)
	}
}
