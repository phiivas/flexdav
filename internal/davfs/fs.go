// Package davfs implements golang.org/x/net/webdav.FileSystem on top of
// the Plex API, so a remote Plex library can be mounted read-only via
// rclone's webdav backend without ever keeping a full local copy.
//
// The exposed tree mirrors Plex's own hierarchy:
//
//	/Movies - 4K/The Matrix (1999) Bluray-2160p.mkv
//	/TV Shows - 4K/Yellowstone/Season 3/Yellowstone - S03E07.mkv
//
// Naming invariant: listing a directory and looking a child up by name
// both go through buildEntries, so whatever Readdir reports is exactly
// what resolve accepts. Getting these out of step is what makes a
// browsable-but-unopenable mount, so they share one code path.
package davfs

import (
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/webdav"

	"github.com/phiivas/flexdav/internal/plex"
)

// ErrReadOnly is returned by every mutating operation.
var ErrReadOnly = errors.New("flexdav: filesystem is read-only")

const (
	// firstChunkSize is how much a fresh read fetches before ramping
	// up, keeping the first bytes after a seek quick to arrive.
	firstChunkSize = 1 << 20 // 1 MiB
	// defaultMaxChunk and defaultStreams size the read pipeline. Four
	// streams is where this CDN peaks; see chunkreader.go.
	defaultMaxChunk = 8 << 20 // 8 MiB
	defaultStreams  = 4

	// branchTimeout bounds one attempt at listing a show's seasons while
	// another server still holds the same series. Deep pagination is
	// slow, so this is generous; it exists to catch a server that has
	// gone away, not one that is merely busy.
	branchTimeout = 45 * time.Second

	// maxReadRetries bounds consecutive failures fetching one chunk.
	maxReadRetries = 5
	// retriesBeforeFailover applies instead while another server still
	// holds the same file. Retrying a dead provider five times costs
	// minutes (each attempt can sit out a 120s header timeout), and the
	// point of a second copy is to be reached before the player gives
	// up. The last source in the chain still gets the full count: there
	// is nowhere else to go, so patience is all that is left.
	retriesBeforeFailover = 2
	baseBackoff           = 200 * time.Millisecond
	maxBackoff            = 5 * time.Second
)

type FS struct {
	// sources are the upstream servers in priority order. With one
	// source this behaves as it always did, except that a film filed
	// under two sections of the same server now appears once.
	sources []*Source
	cat     *catalogueState
	// buildMu serialises catalogue construction. Without it the watcher
	// can start a second full build while the first is still running.
	buildMu  sync.Mutex
	cacheTTL time.Duration
	// allowed, when non-empty, restricts which library sections are
	// exposed (lowercased titles). Plex libraries here run to ~99k
	// items; a PROPFIND over one of those produces an enormous XML
	// body, so mounting only the sections you actually want matters.
	allowed map[string]bool

	streams  int
	maxChunk int64

	// listingsBuilt counts how often an entry list was constructed from
	// scratch. Tests assert that stat-ing entries does not bump it.
	listingsBuilt atomic.Int64

	mu    sync.Mutex
	cache map[string]cacheEntry
	// sectionStamps is the last seen updatedAt per source and section
	// key, used to spot which libraries actually changed.
	sectionStamps map[srcSection]int64
	// dirty are the sections seen to change since the last refresh.
	dirty map[srcSection]bool

	// onChange notifies whatever is caching above us. See Options.
	onChange func(context.Context, []string)

	// attempt is the last catalogue a build produced, published or not.
	// A build that dies part way through leaves its work here so the
	// next one starts from it instead of from nothing. Guarded by mu.
	attempt *catalogue
}

// lastAttempt is what the previous build managed to list, or nil.
func (fs *FS) lastAttempt() *catalogue {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.attempt
}

// rememberAttempt keeps a build's raw listings for the next one to fall
// back on. It shares the item slices with c rather than copying them.
func (fs *FS) rememberAttempt(c *catalogue) {
	fs.mu.Lock()
	fs.attempt = c
	fs.mu.Unlock()
}

// Options configures the filesystem.
type Options struct {
	// CacheTTL is how long library listings are reused.
	CacheTTL time.Duration
	// Sections, when non-empty, limits which library titles are shown.
	Sections []string
	// Streams is how many ranged GETs run concurrently per open file.
	Streams int
	// MaxChunk is the largest single ranged GET.
	MaxChunk int64
	// OnChange, if set, is called with the titles of sections Plex
	// reports as changed. It exists so a cache above this one (an
	// rclone mount, in practice) can be told to forget exactly those
	// directories, which is what makes a very long --dir-cache-time
	// safe rather than reckless.
	OnChange func(context.Context, []string)
}

type cacheEntry struct {
	sections []plex.Directory
	listing  *dirListing
	at       time.Time
}

// dirListing is a directory's children with a name index beside them.
//
// The index is not an optimisation, it is what makes large libraries
// usable at all. Clients stat every entry they list, and rebuilding the
// entry slice and scanning it on each stat is quadratic: the 99,110
// item "Movies" section came to roughly 10^10 operations and never
// finished listing. Built once, cached, looked up in constant time.
type dirListing struct {
	entries []entry
	byName  map[string]int
}

func listingFrom(entries []entry) *dirListing {
	byName := make(map[string]int, len(entries))
	for i, e := range entries {
		byName[e.name] = i
	}
	return &dirListing{entries: entries, byName: byName}
}

func (l *dirListing) find(name string) *entry {
	if i, ok := l.byName[name]; ok {
		return &l.entries[i]
	}
	return nil
}

// New builds the filesystem over a single server.
func New(client *plex.Client, opts Options) *FS {
	if opts.Streams < 1 {
		opts.Streams = defaultStreams
	}
	if opts.MaxChunk < firstChunkSize {
		opts.MaxChunk = defaultMaxChunk
	}
	return NewMulti([]*Source{{
		Name:     "plex",
		Client:   client,
		Streams:  opts.Streams,
		MaxChunk: opts.MaxChunk,
	}}, opts)
}

// NewMulti builds the filesystem over several servers, merged into one
// tree. Sources are given in priority order: when the same work is on
// more than one, the earliest source wins. See catalogue.go.
func NewMulti(sources []*Source, opts Options) *FS {
	allowed := make(map[string]bool, len(opts.Sections))
	for _, s := range opts.Sections {
		if s = strings.TrimSpace(s); s != "" {
			allowed[strings.ToLower(s)] = true
		}
	}
	for _, s := range sources {
		if s.Streams < 1 {
			s.Streams = defaultStreams
		}
		if s.MaxChunk < firstChunkSize {
			s.MaxChunk = defaultMaxChunk
		}
	}
	return &FS{
		sources:       sources,
		cat:           newCatalogueState(),
		cacheTTL:      opts.CacheTTL,
		allowed:       allowed,
		streams:       opts.Streams,
		maxChunk:      opts.MaxChunk,
		cache:         make(map[string]cacheEntry),
		sectionStamps: make(map[srcSection]int64),
		dirty:         make(map[srcSection]bool),
		onChange:      opts.OnChange,
	}
}

// Build constructs the catalogue. It is slow (minutes, hundreds of
// thousands of records) and is meant to be called once at startup,
// before or alongside serving; requests arriving meanwhile block.
func (fs *FS) Build(ctx context.Context) error {
	fs.buildMu.Lock()
	defer fs.buildMu.Unlock()

	c, err := fs.buildCatalogue(ctx)
	if err != nil && c == nil {
		return err
	}
	n := titleCount(c)

	// An incomplete build must never replace a fuller one. Published, a
	// short catalogue does not read as "a server is down" to the scanner
	// above: it reads as the missing titles having been deleted.
	//
	// This is not hypothetical. With a source unreachable for a whole
	// build, and mirrored mode meaning its sections are the only ones,
	// the result publishes cleanly as "0 sections, 0 titles" while the
	// local Plex hammers the mount for files the bridge has just started
	// denying.
	if c.partial {
		if prev := fs.cat.get(); prev != nil {
			log.Printf("flexdav: build reached %d sections and %d titles but a server was unreachable; keeping the previous %d and %d",
				len(c.sections), n, len(prev.sections), titleCount(prev))
			return ErrPartialCatalogue
		}
		if n == 0 {
			// Nothing to fall back on and nothing worth publishing.
			// Callers stay blocked, which is the honest answer: the
			// library is unknown right now, not empty.
			log.Print("flexdav: a server was unreachable and there is nothing to serve yet; not publishing an empty catalogue")
			return ErrPartialCatalogue
		}
	}

	fs.cat.set(c)
	// A full build re-listed everything, so nothing is outstanding.
	fs.mu.Lock()
	fs.dirty = make(map[srcSection]bool)
	fs.mu.Unlock()
	log.Printf("flexdav: catalogue ready: %d sections, %d titles", len(c.sections), n)
	if c.partial {
		// Published because it is the first thing we have and it does
		// hold titles, but say so: one server's library is missing and
		// the caller should keep trying.
		return ErrPartialCatalogue
	}
	if c.carried > 0 {
		// Every section is present and its contents are sound, so this
		// is served as it stands. It is still worth building again once
		// the server answers: a section added or removed while it was
		// away is invisible to a listing taken before the outage, and
		// nothing else in the process ever does a full build.
		log.Printf("flexdav: %d section(s) came from the previous attempt; will build again for a first-hand listing", c.carried)
		return ErrStaleCatalogue
	}
	return nil
}

// titleCount is how many entries a catalogue exposes in total.
func titleCount(c *catalogue) int {
	n := 0
	for _, l := range c.listings {
		n += len(l.entries)
	}
	return n
}

// ErrPartialCatalogue means the catalogue was built and published, but a
// server was unreachable and its titles are absent.
var ErrPartialCatalogue = errors.New("flexdav: catalogue is missing an unreachable server")

// ErrStaleCatalogue means the catalogue was built and published in full,
// but some of it is the previous attempt's listing because the server
// would not answer this time.
var ErrStaleCatalogue = errors.New("flexdav: catalogue reuses an earlier listing for an unreachable server")

func (fs *FS) Mkdir(context.Context, string, os.FileMode) error { return ErrReadOnly }
func (fs *FS) RemoveAll(context.Context, string) error          { return ErrReadOnly }
func (fs *FS) Rename(context.Context, string, string) error     { return ErrReadOnly }

// --- naming ---------------------------------------------------------

// sanitize makes a Plex title safe to use as a single path component.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.TrimSpace(s)
	switch s {
	case "", ".", "..":
		return "_"
	}
	return s
}

// primaryPart returns the item's first media Part, or nil for branch
// items (shows, seasons) that hold no file of their own.
func primaryPart(it plex.Item) *plex.Part {
	if len(it.Media) == 0 || len(it.Media[0].Part) == 0 {
		return nil
	}
	return &it.Media[0].Part[0]
}

// displayName prefers the real server-side filename when Plex exposes
// it, since release names ("...S03E07 [WEBDL-2160p...]-NTb.mkv") are
// what Sonarr and Radarr know how to parse. Otherwise it falls back to
// the item title plus the container extension.
func displayName(it plex.Item, p *plex.Part) string {
	if p == nil {
		return sanitize(it.Title) + idSuffix(it)
	}
	if p.File != "" {
		if b := path.Base(strings.ReplaceAll(p.File, `\`, "/")); b != "" && b != "." && b != "/" {
			return withID(sanitize(b), it)
		}
	}
	ext := p.Container
	if ext == "" {
		ext = "bin"
	}
	return sanitize(it.Title) + idSuffix(it) + "." + ext
}

// nameIDs writes the external id into every exposed name.
//
// The upstream servers already know each title's TMDb/TVDB/IMDb id and
// hand it over for free in the same bulk listing this bridge already
// makes. Without it in the name, the local Plex has to search its agent
// for every one of 52,022 series and 190,000 films by title and guess
// which result is right; with it, there is nothing to search. Both
// `{tvdb-N}` and `{tmdb-N}` are read by the agents in use.
//
// Off by default, because turning it on renames every path. A library
// that has already been scanned would see that as its entire contents
// being deleted and replaced, so this is a decision to take before the
// first real scan, never after one.
var nameIDs = os.Getenv("PLEX_NAME_IDS") != ""

// idSuffix is the marker for one item, empty when there is nothing
// useful to say. Only whole works carry one: seasons and episodes are
// matched by their position inside an already-identified show, so an id
// there would be noise and would rename far more paths than it helps.
func idSuffix(it plex.Item) string {
	if !nameIDs {
		return ""
	}
	// TVDB first for television, TMDb first for film: that is the order
	// each agent trusts, and a wrong-namespace id is worse than none.
	var order []string
	switch it.Type {
	case "show":
		order = []string{"tvdb", "tmdb", "imdb"}
	case "movie":
		order = []string{"tmdb", "imdb"}
	default:
		return ""
	}
	for _, src := range order {
		if id := it.ExternalID(src); id != "" {
			return " {" + src + "-" + id + "}"
		}
	}
	return ""
}

// withID inserts the marker before the extension, so a release name
// stays parseable by Sonarr and Radarr, which read what is around it.
// Names that already carry an id are left alone: one provider writes
// "[imdb-tt1181849][tmdb-79451]" into its filenames itself, and a second
// copy of the same fact helps nobody.
func withID(name string, it plex.Item) string {
	suffix := idSuffix(it)
	if suffix == "" || hasID(name) {
		return name
	}
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + suffix + ext
}

func hasID(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range []string{"tmdb-", "tvdb-", "imdb-"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// entry is one child of a directory together with the name it is
// exposed under.
//
// src and sectionKey travel with the entry because with more than one
// upstream server a directory's children can come from different places:
// the merged "Movies" holds films won by either server. Whoever opens a
// file or descends into a show needs to know which server to ask, and
// asking the wrong one produces a listing that browses but will not
// open.
type entry struct {
	name       string
	item       plex.Item
	part       *plex.Part // non-nil => regular file
	src        int         // index into FS.sources
	sectionKey string      // the section key on that server
	alts       []altCopy   // identical copies on other servers
	altDirs    []altBranch // the same show on other servers
}

func (e entry) isDir() bool { return e.part == nil }

func (e entry) size() int64 {
	if e.part != nil {
		return e.part.Size
	}
	return 0
}

// buildEntries assigns each item its exposed name, disambiguating
// collisions (Plex happily holds two distinct shows with one title).
// It is deterministic for a given slice, which is what keeps listing
// and lookup agreeing.
func buildEntries(items []plex.Item) []entry {
	out := make([]entry, 0, len(items))
	counts := make(map[string]int, len(items))
	for _, it := range items {
		p := primaryPart(it)
		name := displayName(it, p)
		key := strings.ToLower(name)
		counts[key]++
		if n := counts[key]; n > 1 {
			name = insertSuffix(name, n)
		}
		out = append(out, entry{name: name, item: it, part: p})
	}
	return out
}

// insertSuffix turns "Title.mkv" into "Title (2).mkv".
func insertSuffix(name string, n int) string {
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + " (" + strconv.Itoa(n) + ")" + ext
}

func sectionEntries(sections []plex.Directory) []entry {
	out := make([]entry, 0, len(sections))
	counts := make(map[string]int, len(sections))
	for _, s := range sections {
		name := sanitize(s.Title)
		key := strings.ToLower(name)
		counts[key]++
		if n := counts[key]; n > 1 {
			name = insertSuffix(name, n)
		}
		out = append(out, entry{name: name, item: plex.Item{Title: s.Title, Key: s.Key}})
	}
	return out
}

// --- caching --------------------------------------------------------

// sectionsListing is the root: the merged set of library sections.
func (fs *FS) sectionsListing(ctx context.Context) (*dirListing, error) {
	c, err := fs.cat.wait(ctx)
	if err != nil {
		return nil, err
	}
	return c.sectionList(), nil
}

func (fs *FS) filterSections(sections []plex.Directory) []plex.Directory {
	if len(fs.allowed) == 0 {
		return sections
	}
	out := make([]plex.Directory, 0, len(fs.allowed))
	for _, s := range sections {
		if fs.allowed[strings.ToLower(s.Title)] {
			out = append(out, s)
		}
	}
	return out
}

// cachedListing returns a directory's children, building the entry list
// and its name index once and reusing both until the TTL expires.
func (fs *FS) cachedListing(ctx context.Context, key string, src int, sectionKey string, fetch func() ([]plex.Item, error)) (*dirListing, error) {
	fs.mu.Lock()
	if e, ok := fs.cache[key]; ok && e.listing != nil && time.Since(e.at) < fs.cacheTTL {
		fs.mu.Unlock()
		return e.listing, nil
	}
	fs.mu.Unlock()

	items, err := fetch()
	if err != nil {
		return nil, err
	}
	entries := buildEntries(items)
	// Children inherit their parent's server: an episode is only ever
	// available from the server whose show won.
	for i := range entries {
		entries[i].src = src
		entries[i].sectionKey = sectionKey
	}
	l := listingFrom(entries)
	fs.listingsBuilt.Add(1)

	fs.mu.Lock()
	fs.cache[key] = cacheEntry{listing: l, at: time.Now()}
	fs.mu.Unlock()
	return l, nil
}

// sectionListing returns a merged section's winning entries. It comes
// straight out of the catalogue rather than from the API, because
// deciding what belongs here needs every server's whole catalogue, not
// just this section.
func (fs *FS) sectionListing(ctx context.Context, sectionName string) (*dirListing, error) {
	c, err := fs.cat.wait(ctx)
	if err != nil {
		return nil, err
	}
	if l, ok := c.listings[sectionName]; ok {
		return l, nil
	}
	// A section with no winners at all is still a real, empty directory.
	return listingFrom(nil), nil
}

// childListing keys the cache by source and section as well as item, so
// that when a section changes everything cached beneath it can be
// dropped together, and so that two servers' identically numbered
// sections never collide.
func (fs *FS) childListing(ctx context.Context, src int, sectionKey, ratingKey string) (*dirListing, error) {
	key := "children:" + strconv.Itoa(src) + ":" + sectionKey + ":" + ratingKey
	return fs.cachedListing(ctx, key, src, sectionKey, func() ([]plex.Item, error) {
		return fs.sources[src].Client.ListChildren(ctx, ratingKey)
	})
}

// childListingOrAlt lists a show's children, falling back to another
// server that knows the same series when the chosen one will not answer.
//
// This is the difference between a library that goes quiet during an
// outage and one that takes the whole Plex database down with it: a
// directory that never returns leaves the scanner blocked, and a blocked
// scanner holds locks every other section needs.
//
// The context deadline matters as much as the fallback. Without one, a
// provider that swallows packets leaves this waiting on the client's own
// timeouts, by which time the caller has given up anyway.
func (fs *FS) childListingOrAlt(ctx context.Context, src int, sectionKey, ratingKey string, alts []altBranch) (*dirListing, error) {
	tries := append([]altBranch{{src: src, sectionKey: sectionKey, ratingKey: ratingKey}}, alts...)
	var firstErr error
	for i, t := range tries {
		attempt := ctx
		if len(tries) > 1 && i < len(tries)-1 {
			// Only bound the attempts that have somewhere to fall back
			// to. The last one is all that is left, so let it take as
			// long as the caller allows.
			var cancel context.CancelFunc
			attempt, cancel = context.WithTimeout(ctx, branchTimeout)
			defer cancel()
		}
		l, err := fs.childListing(attempt, t.src, t.sectionKey, t.ratingKey)
		if err == nil {
			if i > 0 {
				log.Printf("flexdav: %s did not answer for a show, listed it from %s instead",
					fs.sources[src].Name, fs.sources[t.src].Name)
			}
			return l, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// Watch polls Plex for section changes and drops the cached listings of
// whichever sections actually changed, so new episodes and titles turn
// up without waiting for the whole cache to time out.
//
// Listing sections is cheap (~11 KB) and carries each section's
// updatedAt, so this costs one small request per interval no matter how
// large the libraries are.
func (fs *FS) Watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var changed []string
			for si, s := range fs.sources {
				sections, err := s.Client.ListSections(ctx)
				if err != nil {
					// A server that is down has not changed as far as
					// anyone can tell. Leave its stamps alone so that
					// coming back does not read as a mass update.
					continue
				}
				changed = append(changed, fs.invalidateChanged(si, fs.filterSections(sections))...)
			}
			if len(changed) == 0 {
				continue
			}
			// Re-list only what moved, then work out the winners again.
			// Re-deriving is not optional even for a single changed
			// section: a new film there can take a title away from a
			// section that did not change.
			if err := fs.refresh(ctx); err != nil {
				log.Printf("flexdav: refresh failed: %v", err)
				continue
			}
			// Deliberately outside the lock: the callback talks to
			// rclone over HTTP and must not hold up reads.
			if fs.onChange != nil {
				fs.onChange(ctx, dedupStrings(changed))
			}
		}
	}
}

// refresh re-lists the sections whose stamps moved and rebuilds the
// deduplicated tree from the raw listings already held.
func (fs *FS) refresh(ctx context.Context) error {
	cur := fs.cat.get()
	if cur == nil {
		// The first build is still running and will pick these up on
		// its own. Starting another one here is what actually happened:
		// the watcher fired five minutes in, saw eight changed sections
		// and kicked off a second full build alongside the first, so
		// both servers were listed twice over for nothing.
		return nil
	}

	fs.buildMu.Lock()
	defer fs.buildMu.Unlock()

	fs.mu.Lock()
	stale := make([]srcSection, 0, len(fs.dirty))
	for ss := range fs.dirty {
		stale = append(stale, ss)
	}
	fs.dirty = make(map[srcSection]bool)
	fs.mu.Unlock()

	// srcNames and extra have to come along. Without srcNames, derive
	// labels a fallback server's own sections "server1" instead of by
	// name, so its titles are filed under a section that does not exist
	// and vanish; without extra, the now-empty directories are not
	// pruned either. Both are set only by a full build, so a refresh
	// that dropped them would empty every extra section five minutes in.
	next := &catalogue{
		sections: cur.sections,
		srcNames: cur.srcNames,
		extra:    cur.extra,
		order:    cur.order,
		raw:      make(map[srcSection][]plex.Item, len(cur.raw)),
		meta:     make(map[srcSection]sectionRef, len(cur.meta)),
	}
	for k, v := range cur.raw {
		next.raw[k] = v
	}
	for k, v := range cur.meta {
		next.meta[k] = v
	}

	for _, ss := range stale {
		items, err := fs.sources[ss.src].Client.ListSectionAll(ctx, ss.key)
		if err != nil {
			// Keep the previous listing for this section rather than
			// dropping it: a stale directory beats a vanished one.
			log.Printf("flexdav: %s: cannot re-list section %s: %v",
				fs.sources[ss.src].Name, ss.key, err)
			continue
		}
		next.raw[ss] = items
	}
	next.derive()
	fs.cat.set(next)
	// Newer than whatever the last full build left behind, so a build
	// that has to fall back later falls back on this instead.
	fs.rememberAttempt(next)
	return nil
}

func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// invalidateChanged drops the cached listings of sections Plex reports as
// changed and returns their titles. The titles matter because they are
// the directory names a mount above sees, so whatever is caching on top
// can be told exactly what to forget instead of everything.
func (fs *FS) invalidateChanged(src int, sections []plex.Directory) []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	changed := make([]srcSection, 0, len(sections))
	titles := make([]string, 0, len(sections))
	for _, s := range sections {
		ss := srcSection{src: src, key: s.Key}
		if prev, ok := fs.sectionStamps[ss]; !ok || prev != s.UpdatedAt {
			if ok {
				changed = append(changed, ss)
				titles = append(titles, sanitize(s.Title))
			}
			fs.sectionStamps[ss] = s.UpdatedAt
		}
	}
	if len(changed) == 0 {
		return nil
	}

	for _, ss := range changed {
		fs.dirty[ss] = true
		prefix := "children:" + strconv.Itoa(ss.src) + ":" + ss.key + ":"
		for k := range fs.cache {
			if strings.HasPrefix(k, prefix) {
				delete(fs.cache, k)
			}
		}
	}
	log.Printf("flexdav: %s: sections changed: %s",
		fs.sources[src].Name, strings.Join(titles, ", "))
	return titles
}

// --- resolution -----------------------------------------------------

type node struct {
	isDir   bool
	name    string
	part    *plex.Part // when file
	src     int        // which source serves it
	alts    []altCopy  // identical copies to fall back on
	modTime time.Time
	// load fetches this directory's children, and is called only when
	// something actually reads the directory.
	//
	// Stat must never trigger it. Listing a section of 1833 shows makes
	// the client stat every entry, and eagerly loading each show's
	// seasons turned that into 1833 serial API calls: the file browser
	// simply hung.
	load func(context.Context) ([]entry, error)
}

func splitPath(name string) []string {
	name = strings.Trim(path.Clean("/"+name), "/")
	if name == "" {
		return nil
	}
	return strings.Split(name, "/")
}

func (fs *FS) resolve(ctx context.Context, name string) (*node, error) {
	parts := splitPath(name)

	secs, err := fs.sectionsListing(ctx)
	if err != nil {
		return nil, err
	}

	// Root lists the library sections.
	if len(parts) == 0 {
		return &node{isDir: true, name: "/", load: func(context.Context) ([]entry, error) {
			return secs.entries, nil
		}}, nil
	}

	sec := secs.find(parts[0])
	if sec == nil {
		return nil, os.ErrNotExist
	}
	sectionName := sec.name

	if len(parts) == 1 {
		return &node{isDir: true, name: sec.name, load: func(ctx context.Context) ([]entry, error) {
			l, err := fs.sectionListing(ctx, sectionName)
			if err != nil {
				return nil, err
			}
			return l.entries, nil
		}}, nil
	}

	l, err := fs.sectionListing(ctx, sectionName)
	if err != nil {
		return nil, err
	}

	cur := l.find(parts[1])
	if cur == nil {
		return nil, os.ErrNotExist
	}

	for idx := 2; idx < len(parts); idx++ {
		if !cur.isDir() {
			// Path continues past a file, e.g. /Movies/x.mkv/y.
			return nil, os.ErrNotExist
		}
		cl, err := fs.childListingOrAlt(ctx, cur.src, cur.sectionKey, cur.item.RatingKey, cur.altDirs)
		if err != nil {
			return nil, err
		}
		next := cl.find(parts[idx])
		if next == nil {
			return nil, os.ErrNotExist
		}
		cur = next
	}

	if !cur.isDir() {
		return &node{
			isDir:   false,
			name:    cur.name,
			part:    cur.part,
			src:     cur.src,
			alts:    cur.alts,
			modTime: cur.item.ModTime(),
		}, nil
	}

	src, sectionKey, ratingKey, altDirs := cur.src, cur.sectionKey, cur.item.RatingKey, cur.altDirs
	return &node{
		isDir:   true,
		name:    cur.name,
		src:     src,
		modTime: cur.item.ModTime(),
		load: func(ctx context.Context) ([]entry, error) {
			cl, err := fs.childListingOrAlt(ctx, src, sectionKey, ratingKey, altDirs)
			if err != nil {
				return nil, err
			}
			return cl.entries, nil
		},
	}, nil
}

// --- os.FileInfo ----------------------------------------------------

type fileInfo struct {
	name    string
	size    int64
	dir     bool
	modTime time.Time
}

func (fi fileInfo) Name() string       { return fi.name }
func (fi fileInfo) Size() int64        { return fi.size }
func (fi fileInfo) ModTime() time.Time { return fi.modTime }
func (fi fileInfo) IsDir() bool        { return fi.dir }
func (fi fileInfo) Sys() any           { return nil }

func (fi fileInfo) Mode() os.FileMode {
	if fi.dir {
		return os.ModeDir | 0o555
	}
	return 0o444
}

// mediaTypes covers what actually turns up in a Plex library. Go's
// built-in MIME table is tiny and a slim container image carries no
// /etc/mime.types, so without this .mkv resolves to nothing.
var mediaTypes = map[string]string{
	".mkv":  "video/x-matroska",
	".mp4":  "video/mp4",
	".m4v":  "video/x-m4v",
	".avi":  "video/x-msvideo",
	".mov":  "video/quicktime",
	".wmv":  "video/x-ms-wmv",
	".webm": "video/webm",
	".ts":   "video/mp2t",
	".m2ts": "video/mp2t",
	".mpg":  "video/mpeg",
	".mpeg": "video/mpeg",
	".flv":  "video/x-flv",
	".iso":  "application/x-iso9660-image",
	".mp3":  "audio/mpeg",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".ac3":  "audio/ac3",
	".srt":  "application/x-subrip",
	".ass":  "text/x-ssa",
	".ssa":  "text/x-ssa",
	".sub":  "text/plain",
	".idx":  "text/plain",
	".vtt":  "text/vtt",
}

// ContentType is the reason a PROPFIND over a large library completes
// at all.
//
// webdav's default findContentType, given a FileInfo that cannot answer
// for itself, OPENS the file and sniffs 512 bytes. Every one of those is
// a ranged HTTP GET to the CDN, issued serially, so listing 877 movies
// meant 877 round trips: measured at over 150 seconds for a single
// directory before this existed. Answering from the extension keeps
// listings metadata-only.
func (fi fileInfo) ContentType(context.Context) (string, error) {
	if fi.dir {
		return "", nil
	}
	ext := strings.ToLower(path.Ext(fi.name))
	if ct, ok := mediaTypes[ext]; ok {
		return ct, nil
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct, nil
	}
	return "application/octet-stream", nil
}

func (fs *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	n, err := fs.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	fi := fileInfo{name: n.name, dir: n.isDir, modTime: n.modTime}
	if !n.isDir {
		fi.size = n.part.Size
	}
	return fi, nil
}

func (fs *FS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, ErrReadOnly
	}
	n, err := fs.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	if n.isDir {
		return &dirHandle{ctx: ctx, node: n}, nil
	}
	// Read settings follow the server the file came from, not a global
	// default: one provider wants four streams of 32 MiB, the other
	// eight of 16 MiB, and using one server's numbers against the other
	// costs most of the throughput.
	src := fs.sources[n.src]
	srcs := make([]readSource, 0, 1+len(n.alts))
	srcs = append(srcs, readSource{name: src.Name, client: src.Client, part: *n.part})
	for _, a := range n.alts {
		if a.src < 0 || a.src >= len(fs.sources) {
			continue
		}
		alt := fs.sources[a.src]
		srcs = append(srcs, readSource{name: alt.Name, client: alt.Client, part: a.part})
	}
	return &fileHandle{
		name:    n.name,
		part:    *n.part,
		modTime: n.modTime,
		reader:  newChunkReader(srcs, src.Streams, src.MaxChunk),
	}, nil
}

// --- directory handle -----------------------------------------------

type dirHandle struct {
	ctx     context.Context
	node    *node
	entries []entry
	loaded  bool
	pos     int
}

// ensure pulls the children on first read, so merely stat-ing a
// directory costs nothing.
func (d *dirHandle) ensure() error {
	if d.loaded {
		return nil
	}
	entries, err := d.node.load(d.ctx)
	if err != nil {
		return err
	}
	d.entries, d.loaded = entries, true
	return nil
}

func (d *dirHandle) Close() error              { return nil }
func (d *dirHandle) Read([]byte) (int, error)  { return 0, io.EOF }
func (d *dirHandle) Write([]byte) (int, error) { return 0, ErrReadOnly }
func (d *dirHandle) Seek(int64, int) (int64, error) {
	return 0, errors.New("flexdav: cannot seek a directory")
}

func (d *dirHandle) Stat() (os.FileInfo, error) {
	return fileInfo{name: d.node.name, dir: true, modTime: d.node.modTime}, nil
}

// Readdir follows the os.File contract: count <= 0 returns everything
// in one slice, count > 0 returns at most count and io.EOF once drained.
func (d *dirHandle) Readdir(count int) ([]os.FileInfo, error) {
	if err := d.ensure(); err != nil {
		return nil, err
	}
	remaining := d.entries[d.pos:]
	if count <= 0 {
		d.pos = len(d.entries)
		return infos(remaining), nil
	}
	if len(remaining) == 0 {
		return nil, io.EOF
	}
	if count > len(remaining) {
		count = len(remaining)
	}
	d.pos += count
	return infos(remaining[:count]), nil
}

func infos(entries []entry) []os.FileInfo {
	out := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, fileInfo{
			name:    e.name,
			size:    e.size(),
			dir:     e.isDir(),
			modTime: e.item.ModTime(),
		})
	}
	return out
}

// --- file handle ----------------------------------------------------

// fileHandle turns Seek+Read into ranged reads against the Plex Part,
// served by a chunkReader that keeps several fetches in flight.
type fileHandle struct {
	name    string
	part    plex.Part
	modTime time.Time

	offset int64
	reader *chunkReader
}

func (f *fileHandle) Stat() (os.FileInfo, error) {
	return fileInfo{name: f.name, size: f.part.Size, modTime: f.modTime}, nil
}

func (f *fileHandle) Write([]byte) (int, error) { return 0, ErrReadOnly }

// Readdir satisfies webdav.File; reading a regular file as a directory
// is an error, matching os.File's behaviour.
func (f *fileHandle) Readdir(int) ([]os.FileInfo, error) {
	return nil, &os.PathError{Op: "readdir", Path: f.name, Err: errors.New("not a directory")}
}

func (f *fileHandle) Close() error {
	f.reader.stop()
	return nil
}

func (f *fileHandle) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.offset + offset
	case io.SeekEnd:
		abs = f.part.Size + offset
	default:
		return 0, errors.New("flexdav: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("flexdav: negative seek position")
	}
	f.offset = abs
	return abs, nil
}

// Read serves bytes at the current offset from the chunk pipeline,
// rebuilding it only when a seek lands somewhere the buffered chunks
// cannot cover.
func (f *fileHandle) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if f.offset >= f.part.Size {
		return 0, io.EOF
	}
	if !f.reader.seekTo(f.offset) {
		f.reader.restart(f.offset)
	}
	n, err := f.reader.read(p)
	f.offset += int64(n)
	if n > 0 {
		// Report any error on the following call, per io.Reader.
		return n, nil
	}
	return 0, err
}

func backoff(attempt int) time.Duration {
	d := baseBackoff << (attempt - 1)
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}
