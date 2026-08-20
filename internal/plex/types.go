package plex

import (
	"encoding/json"
	"strings"
	"time"
)

// Minimal subset of the Plex API JSON response shapes we actually need.

type mediaContainer[T any] struct {
	MediaContainer T `json:"MediaContainer"`
}

// Directory is a library section, e.g. "Movies - 4K-dv".
type Directory struct {
	Key   string `json:"key"` // section key, e.g. "13"
	Title string `json:"title"`
	Type  string `json:"type"` // "movie" | "show"
	// UpdatedAt bumps whenever the section's contents change. Listing
	// all sections costs ~11 KB, so polling this is a cheap way to spot
	// new titles without re-listing anything.
	UpdatedAt int64 `json:"updatedAt"`
	ScannedAt int64 `json:"scannedAt"`
}

type sectionsResponse struct {
	Directory []Directory `json:"Directory"`
}

// Item is a show, season, movie or episode as returned by
// /library/sections/{key}/all or /library/metadata/{ratingKey}/children.
// Leaf items (movies, episodes) carry Media/Part; branch items do not.
type Item struct {
	RatingKey   string  `json:"ratingKey"`
	Key         string  `json:"key"`
	Title       string  `json:"title"`
	Type        string  `json:"type"` // "show" | "season" | "episode" | "movie"
	Year        int     `json:"year"`
	Index       int     `json:"index"`
	ParentIndex int     `json:"parentIndex"`
	LeafCount   int     `json:"leafCount"`
	AddedAt     int64   `json:"addedAt"`   // unix seconds
	UpdatedAt   int64   `json:"updatedAt"` // unix seconds
	Media       []Media `json:"Media"`
	// Guid carries the external identifiers (imdb, tmdb, tvdb) that make
	// it possible to tell that two differently named files on two
	// different servers are the same film. Plex only fills this in when
	// asked with includeGuids=1, which costs nothing measurable: 2.2 MB
	// per 1000 records either way.
	Guid GuidList `json:"Guid"`
}

// GuidRef is one external identifier, e.g. {"id": "tmdb://603"}.
type GuidRef struct {
	ID string `json:"id"`
}

// GuidList tolerates the two different things Plex puts under this name.
//
// A record carries both "guid", a single string like
// "plex://movie/5d77...", and "Guid", an array of external identifiers.
// Go matches JSON keys to struct fields case-insensitively when there is
// no exact match, so the string lands in this field too and decoding
// fails with "cannot unmarshal string into []plex.GuidRef". That killed
// the first real catalogue build outright.
//
// A string is therefore ignored rather than treated as an error, and
// ignored without clearing anything: the two keys can arrive in either
// order, and a plain "return nil" would let a trailing "guid" wipe the
// array that "Guid" had just filled in.
type GuidList []GuidRef

func (g *GuidList) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] == '"' || string(b) == "null" {
		return nil
	}
	var refs []GuidRef
	if err := json.Unmarshal(b, &refs); err != nil {
		return err
	}
	*g = refs
	return nil
}

// ExternalID returns the value of the requested identifier scheme
// ("tmdb", "imdb", "tvdb"), or "" when the server did not supply one.
func (it Item) ExternalID(scheme string) string {
	prefix := scheme + "://"
	for _, g := range it.Guid {
		if strings.HasPrefix(g.ID, prefix) {
			return strings.TrimPrefix(g.ID, prefix)
		}
	}
	return ""
}

// ModTime derives a modification time for WebDAV clients. Plex reports
// these as unix epoch seconds; both may be absent on shared servers, in
// which case we return the zero time and clients fall back to "unknown".
func (it Item) ModTime() time.Time {
	switch {
	case it.UpdatedAt > 0:
		return time.Unix(it.UpdatedAt, 0)
	case it.AddedAt > 0:
		return time.Unix(it.AddedAt, 0)
	default:
		return time.Time{}
	}
}

type Media struct {
	Duration int    `json:"duration"`
	Part     []Part `json:"Part"`
}

// Part is a physical media file on the Plex server.
type Part struct {
	Key string `json:"key"` // e.g. /library/parts/12345/167890/file.mkv
	// File is the server-side absolute path, e.g.
	// /data/movies/The Matrix (1999)/The Matrix (1999) Bluray-1080p.mkv
	// Plex omits this on some shared servers, so treat it as optional;
	// its basename gives far better filenames than Title+container.
	File      string `json:"file"`
	Size      int64  `json:"size"`
	Container string `json:"container"` // extension without dot, e.g. "mkv"
}

type itemsResponse struct {
	Size      int    `json:"size"`
	TotalSize int    `json:"totalSize"`
	Metadata  []Item `json:"Metadata"`
}
