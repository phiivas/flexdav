package davfs

import (
	"testing"

	"github.com/phiivas/flexdav/internal/plex"
)

// withNameIDs turns the id-in-name behaviour on for one test and puts it
// back afterwards, since it is read from the environment at start-up.
func withNameIDs(t *testing.T) {
	t.Helper()
	prev := nameIDs
	nameIDs = true
	t.Cleanup(func() { nameIDs = prev })
}

func item(kind, title string, guids ...string) plex.Item {
	it := plex.Item{Type: kind, Title: title}
	for _, g := range guids {
		it.Guid = append(it.Guid, plex.GuidRef{ID: g})
	}
	return it
}

// The whole point: the local Plex must never have to search for a title
// it was already told the id of. A series folder carries its TVDB id.
func TestShowFolderCarriesItsID(t *testing.T) {
	withNameIDs(t)

	got := displayName(item("show", "Doug", "imdb://tt0106144", "tmdb://2225", "tvdb://72502"), nil)
	if want := "Doug {tvdb-72502}"; got != want {
		t.Errorf("show folder is %q, want %q", got, want)
	}
}

// Television is TVDB's ground and film is TMDb's, and each agent trusts
// its own first. An id from the wrong namespace is worse than none: the
// numbers collide across namespaces, so it would match a real but
// unrelated work.
func TestPreferredNamespaceDiffersByKind(t *testing.T) {
	withNameIDs(t)

	show := displayName(item("show", "Some Series", "tmdb://603", "tvdb://999"), nil)
	if want := "Some Series {tvdb-999}"; show != want {
		t.Errorf("show folder is %q, want %q", show, want)
	}

	film := plex.Part{File: "/x/The Matrix (1999).mkv", Container: "mkv"}
	movie := displayName(item("movie", "The Matrix", "tmdb://603", "imdb://tt0133093"), &film)
	if want := "The Matrix (1999) {tmdb-603}.mkv"; movie != want {
		t.Errorf("film name is %q, want %q", movie, want)
	}
}

// The marker goes before the extension so the release name stays
// readable to Sonarr and Radarr, which parse what surrounds it.
func TestIDGoesBeforeTheExtension(t *testing.T) {
	withNameIDs(t)

	p := plex.Part{File: "/m/Arrival (2016) [WEBDL-1080p x264 EAC3 5.1]-NTb.mkv", Container: "mkv"}
	got := displayName(item("movie", "Arrival", "tmdb://329865"), &p)
	if want := "Arrival (2016) [WEBDL-1080p x264 EAC3 5.1]-NTb {tmdb-329865}.mkv"; got != want {
		t.Errorf("name is %q, want %q", got, want)
	}
}

// One provider already writes ids into its own filenames. Adding a
// second copy of the same fact only makes the path longer.
func TestNameThatAlreadyHasAnIDIsLeftAlone(t *testing.T) {
	withNameIDs(t)

	p := plex.Part{File: "/m/Sharpes Peril (2008) [imdb-tt1181849][tmdb-79451][Bluray-1080p].mkv", Container: "mkv"}
	got := displayName(item("movie", "Sharpes Peril", "tmdb://79451"), &p)
	if want := "Sharpes Peril (2008) [imdb-tt1181849][tmdb-79451][Bluray-1080p].mkv"; got != want {
		t.Errorf("name is %q, want %q", got, want)
	}
}

// Seasons and episodes are matched by their place inside a show that has
// already been identified. Marking them would rename hundreds of
// thousands of paths to no purpose.
func TestSeasonsAndEpisodesAreNotMarked(t *testing.T) {
	withNameIDs(t)

	season := displayName(item("season", "Season 1", "tvdb://72502"), nil)
	if season != "Season 1" {
		t.Errorf("season folder is %q, want %q", season, "Season 1")
	}

	p := plex.Part{File: "/s/Doug - S01E01 - Doug Bags a Neematoad.mkv", Container: "mkv"}
	ep := displayName(item("episode", "Doug Bags a Neematoad", "tvdb://12345"), &p)
	if want := "Doug - S01E01 - Doug Bags a Neematoad.mkv"; ep != want {
		t.Errorf("episode is %q, want %q", ep, want)
	}
}

// A title with no id at all still has to appear, unchanged. About 1% of
// records carry none.
func TestTitleWithoutAnIDIsUnchanged(t *testing.T) {
	withNameIDs(t)

	if got := displayName(item("show", "Some Home Video"), nil); got != "Some Home Video" {
		t.Errorf("show folder is %q, want %q", got, "Some Home Video")
	}
}

// Off by default. Turning it on renames every path, which a scanned
// library reads as its whole contents being replaced, so it must never
// happen by accident.
func TestNamesAreUnmarkedWhenTheFlagIsOff(t *testing.T) {
	prev := nameIDs
	nameIDs = false
	t.Cleanup(func() { nameIDs = prev })

	if got := displayName(item("show", "Doug", "tvdb://72502"), nil); got != "Doug" {
		t.Errorf("show folder is %q, want %q", got, "Doug")
	}
}
