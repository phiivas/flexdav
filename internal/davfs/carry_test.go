package davfs

import (
	"context"
	"errors"
	"testing"
)

func withExtras(t *testing.T) {
	t.Helper()
	prev := extraSections
	extraSections = true
	t.Cleanup(func() { extraSections = prev })
}

// A refresh derives the tree again from the listings already held, and
// it has to carry the source names and the set of extra sections with
// it. Without them the fallback server's own titles are filed under a
// section named after its index, which no directory matches, so five
// minutes after startup every extra library went empty. To Plex an
// emptied library is not a glitch, it is a deletion.
func TestARefreshKeepsTheExtraSections(t *testing.T) {
	withMirror(t)
	withExtras(t)

	a := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("a1", "The Matrix", 1999, "603", 1000),
	}})
	b := newFakeServer(t, fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
		film("b1", "Solaris", 1972, "593", 2000),
	}})
	fs := bareFS(t, a, b)
	if err := fs.Build(context.Background()); err != nil {
		t.Fatalf("build: %v", err)
	}
	const extra = "Movies - s1"
	before := names(t, fs, "/"+extra)
	if len(before) != 1 {
		t.Fatalf("%s lists %v after the build, want the one title only it holds", extra, before)
	}

	// What the watcher does when a section's updatedAt moves.
	fs.mu.Lock()
	fs.dirty[srcSection{src: 0, key: "1"}] = true
	fs.mu.Unlock()
	if err := fs.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := names(t, fs, "/"+extra); len(got) != len(before) {
		t.Errorf("%s lists %v after the refresh, want the %v it had", extra, got, before)
	}
}

// twoSectionServer is one server holding two film libraries, so a test
// can kill one of them and leave the other answering.
func twoSectionServer(t *testing.T) *fakeServer {
	t.Helper()
	return newFakeServer(t,
		fakeSection{key: "1", title: "Movies", typ: "movie", items: []map[string]any{
			film("a1", "The Matrix", 1999, "603", 1000),
		}},
		fakeSection{key: "2", title: "Movies - Kids", typ: "movie", items: []map[string]any{
			film("a2", "Ponyo", 2008, "12429", 2000),
		}},
	)
}

// A build that dies part way through must not throw away what it already
// listed. Against the real library a full build takes about half an hour
// and a source can drop out at any moment, so a build interrupted
// two thirds of the way through is ordinary. Before this, each outage
// discarded the whole half hour, and after two of them in a row nothing
// had ever been published at all.
func TestAFailedSectionFallsBackOnTheEarlierListing(t *testing.T) {
	a := twoSectionServer(t)
	fs := bareFS(t, a)

	// The first attempt gets the second library but not the first.
	a.deadSections = map[string]bool{"1": true}
	if err := fs.Build(context.Background()); !errors.Is(err, ErrPartialCatalogue) {
		t.Fatalf("first build: %v, want ErrPartialCatalogue", err)
	}

	// The second gets the first library but loses the second, which is
	// where the whole thing used to start over.
	a.deadSections = map[string]bool{"2": true}
	err := fs.Build(context.Background())
	if !errors.Is(err, ErrStaleCatalogue) {
		t.Fatalf("second build: %v, want ErrStaleCatalogue", err)
	}
	if got := titleCount(fs.cat.get()); got != 2 {
		t.Errorf("catalogue holds %d titles, want both libraries' 2", got)
	}
	if got := names(t, fs, "/Movies"); len(got) != 1 {
		t.Errorf("Movies lists %v, want the film listed this time", got)
	}
	if got := names(t, fs, "/Movies - Kids"); len(got) != 1 {
		t.Errorf("Movies - Kids lists %v, want the film kept from the earlier attempt", got)
	}
}

// Carrying a section over must not freeze the sections that did answer:
// the point is to keep building on what is there, not to stop building.
func TestCarriedSectionsDoNotHoldBackTheFreshOnes(t *testing.T) {
	a := twoSectionServer(t)
	fs := bareFS(t, a)
	if err := fs.Build(context.Background()); err != nil {
		t.Fatalf("first build: %v", err)
	}

	a.sections[0].items = append(a.sections[0].items, film("a3", "Dune", 2021, "438631", 3000))
	a.deadSections = map[string]bool{"2": true}
	if err := fs.Build(context.Background()); !errors.Is(err, ErrStaleCatalogue) {
		t.Fatalf("build with one dead section: %v, want ErrStaleCatalogue", err)
	}
	if got := names(t, fs, "/Movies"); len(got) != 2 {
		t.Errorf("Movies lists %v, want the new film to have landed", got)
	}
	if got := names(t, fs, "/Movies - Kids"); len(got) != 1 {
		t.Errorf("Movies - Kids lists %v, want the kept film", got)
	}
}

// A clean build must report success, or the caller keeps rebuilding a
// library this size forever.
func TestACleanBuildAfterACarriedOneReportsSuccess(t *testing.T) {
	a := twoSectionServer(t)
	fs := bareFS(t, a)

	a.deadSections = map[string]bool{"2": true}
	if err := fs.Build(context.Background()); !errors.Is(err, ErrPartialCatalogue) {
		t.Fatalf("first build: %v, want ErrPartialCatalogue", err)
	}
	a.deadSections = nil
	if err := fs.Build(context.Background()); err != nil {
		t.Fatalf("build with everything answering: %v, want success", err)
	}
	if got := titleCount(fs.cat.get()); got != 2 {
		t.Errorf("catalogue holds %d titles, want 2", got)
	}
}

// A whole source that cannot even be asked for its section list is
// carried the same way, since that is what a dead server looks like from
// the first request rather than the tenth.
func TestAnUnreachableSourceKeepsItsSectionsFromTheEarlierAttempt(t *testing.T) {
	a := twoSectionServer(t)
	fs := bareFS(t, a)
	if err := fs.Build(context.Background()); err != nil {
		t.Fatalf("first build: %v", err)
	}

	a.down = true
	if err := fs.Build(context.Background()); !errors.Is(err, ErrStaleCatalogue) {
		t.Fatalf("build with the server down: %v, want ErrStaleCatalogue", err)
	}
	c := fs.cat.get()
	if len(c.sections) != 2 {
		t.Fatalf("catalogue exposes %d sections, want both", len(c.sections))
	}
	if c.sections[0].name != "Movies" || c.sections[1].name != "Movies - Kids" {
		t.Errorf("sections came back as %v, want them in the original order", c.sections)
	}
	if got := titleCount(c); got != 2 {
		t.Errorf("catalogue holds %d titles, want 2", got)
	}
}

// A section that has never been listed successfully has nothing to fall
// back on, and must be left out rather than exposed as an empty
// directory. Plex scans an empty library happily and records every file
// in it as deleted.
func TestASectionNeverListedIsNotExposedEmpty(t *testing.T) {
	a := twoSectionServer(t)
	a.deadSections = map[string]bool{"2": true}
	fs := bareFS(t, a)

	if err := fs.Build(context.Background()); !errors.Is(err, ErrPartialCatalogue) {
		t.Fatalf("build: %v, want ErrPartialCatalogue", err)
	}
	for _, s := range fs.cat.get().sections {
		if s.name == "Movies - Kids" {
			t.Error("a section that was never listed is exposed as an empty directory")
		}
	}
}
