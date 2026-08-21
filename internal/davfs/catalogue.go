package davfs

import (
	"context"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phiivas/flexdav/internal/plex"
)

// A catalogue is the union of every configured server's libraries with
// each work appearing exactly once.
//
// Why this exists at all. Two shared Plex servers were measured: roughly
// 150,000 unique films on the first, 94,000 on the second, and some 76,000
// of them the same film on both. Exposing the servers separately
// would make the local Plex index those 76,000 twice, and scanning is by
// far the most expensive thing in this project. It would also show them
// to a viewer as duplicate search results.
//
// Deduplication has to happen across the whole catalogue, not within
// same-named sections. A film sits in "Movies" on one server and in
// "Movies - Remux" on the other, and one server alone had 8,601 films
// filed under more than one section. Merging only matching section names
// would leave most of the duplicates in place.
//
// What a viewer sees: the section directories stay as they are, and each
// film appears in exactly one of them, the winner's. Nothing is
// flattened and no section disappears.

// Source is one upstream Plex server. The order sources are given in is
// their priority: earlier wins when the same work is on both.
//
// Settings are per source because the right ones are a property of the
// provider, not a universal answer. One of these wants HTTP/1.1 and four
// streams, the other wants HTTP/2 and eight, and the gap was sevenfold.
type Source struct {
	Name     string // for logs, e.g. "primary"
	Client   *plex.Client
	Streams  int
	MaxChunk int64
}

// candidate is one server's copy of one work.
type candidate struct {
	src        int    // index into FS.sources, so lower is preferred
	sectionKey string // the section key on that server
	section    string // the section's exposed directory name
	item       plex.Item
	part       *plex.Part  // nil for shows, which hold no file of their own
	alts       []altCopy   // other servers holding the identical file
	altDirs    []altBranch // other servers holding the same show
}

// altCopy is another server's copy of the same file, kept so a read can
// carry on when the chosen server stops answering.
//
// Only byte-for-byte identical lengths qualify. The size is published
// when the directory is listed and the client remembers it, so handing
// over a differently sized file mid-stream breaks seeking and cuts the
// ending off. Of the 76,068 films both servers hold, 18,549 match
// exactly; the rest are different encodings of the same film and cannot
// stand in for one another.
type altCopy struct {
	src  int
	part plex.Part
}

// altBranch is another server's copy of the same *show*, kept so that
// listing its seasons and episodes can carry on when the chosen server
// stops answering.
//
// This is a different problem from altCopy and a much less demanding
// one. A show holds no file, so nothing has to match byte for byte; the
// other server only has to know the series. Where films need identical
// lengths and only 28.8% of the shared ones qualify, 75.6% of shared
// series have at least as many episodes on the second server.
//
// It matters more than the film case, too. A show's seasons are not in
// the catalogue: they are fetched live from whichever server won the
// show. With the winner gone, the folder does not merely fail to play,
// it fails to open at all, and a Plex library sitting on a directory
// that never answers will hold its whole database locked, not just its
// own section. Seen in practice: one such library made
// /library/sections stop answering for every client on the server.
type altBranch struct {
	src        int
	sectionKey string
	ratingKey  string
}

// mirrorPrimary exposes the first source's library and nothing else,
// while still letting the others stand in when it stops answering.
//
// The difference from the merged behaviour is what the second server is
// allowed to contribute. Merged, it adds what the first does not have:
// 17,824 films and 6,627 series, plus its own sections. Mirrored, it
// adds nothing at all and exists only as a fallback, so the tree is the
// primary's library exactly, section for section.
//
// Which is right is not a technical question. A merged tree holds more,
// a mirrored one is predictable: what is in it is what the primary has,
// and nothing appears or vanishes depending on which server answered.
var mirrorPrimary = os.Getenv("PLEX_MIRROR_PRIMARY") != ""

// extraSections keeps what only a fallback server has, but in sections
// of its own rather than folded into the primary's.
//
// Only meaningful alongside mirrorPrimary, and the reason it exists is
// reversibility. Against the real pair the second server adds 18,000
// films and 6,971 series that the first does not have, which is worth
// having; but folded into "Movies" they cannot be taken back out again
// without the local Plex seeing a mass deletion inside a library that
// matters. In their own section they are one library to add and one to
// drop, and the primary's tree never changes either way.
//
// It also lets them be scanned last. The local Plex scans one library at
// a time at about 35 files a minute, so anything added here delays the
// primary's content by exactly its own size.
var extraSections = os.Getenv("PLEX_EXTRA_SECTIONS") != ""

// extraSuffix labels those sections. It follows the same shape the rest
// of the library already uses ("Movies - 3D", "TV Shows - Kids"), so the
// extra ones sort next to their own kind instead of forming a second
// naming scheme. Defaults to the server's own name; set
// PLEX_EXTRA_SUFFIX to something shorter.
var extraSuffix = os.Getenv("PLEX_EXTRA_SUFFIX")

// extraName is what a fallback server's own section is exposed as.
func extraName(section, srcName string) string {
	label := extraSuffix
	if label == "" {
		label = srcName
	}
	return section + " - " + label
}

// sectionRef is a merged section, i.e. one directory at the root.
type sectionRef struct {
	name string // exposed directory name
	typ  string // "movie" | "show"
}

// srcSection identifies one server's one section.
type srcSection struct {
	src int
	key string
}

type catalogue struct {
	sections []sectionRef
	// srcNames names each source, so a fallback server's own sections can
	// be labelled with where they came from.
	srcNames []string
	// extra marks the sections that exist only to hold what a fallback
	// server has and the primary does not, so an empty one can be dropped
	// while an empty section of the primary's stays.
	extra map[string]bool
	// raw holds what each server said, kept so that a single changed
	// section can be re-listed without walking all of them again. The
	// deduplication is then re-derived from raw, which it has to be: a
	// new film in one section can take a title away from another.
	raw map[srcSection][]plex.Item
	// meta remembers each raw key's section name and type.
	meta map[srcSection]sectionRef
	// order records the sections in the order they were listed. A build
	// that falls back on the previous attempt's data replays this rather
	// than walking a map, so the root listing keeps the same order
	// instead of reshuffling under a mount that caches it for days.
	order []srcSection
	// listings is the derived result: exposed section name -> its
	// winning entries, with the name index already built.
	listings map[string]*dirListing
	builtAt  time.Time
	// partial records that at least one server could not be reached, so
	// this catalogue is missing everything that server holds.
	partial bool
	// carried counts sections this build could not list and took from
	// the previous attempt instead. Their contents are sound but a
	// section added or removed while the server was away is not visible,
	// so a build that carried anything is worth repeating.
	carried int
}

// dedupKey identifies a work across servers.
//
// TMDb first because it covered 99.5% of records against 97.4% for IMDb,
// then IMDb, then TVDB, then a normalised title and year.
//
// The two kinds of mistake here are not equal. Failing to merge is
// benign: the film shows up twice, which is exactly what happens today.
// Merging two different films is destructive: one of them disappears
// from the library with nothing to indicate it ever existed. So every
// judgement call below leans towards leaving them apart.
//
// That is why a title with no year and no identifier at all gets a key
// unique to its server and rating key, and is never merged with
// anything. Plex will happily hold two entirely different works under
// one title, and about 1% of records here carry no external id, so
// matching those on title alone would quietly delete films. Matching on
// title *and* year is allowed: it is still a guess, but a much narrower
// one.
//
// A film known by TMDb id on one server and only by IMDb id on the other
// will not merge either, because the two live in different key spaces.
// Same trade: a visible duplicate beats an invisible deletion.
func dedupKey(it plex.Item, src int) string {
	// The kind has to be part of the key. TMDb numbers films and
	// television in two separate sequences, both starting from small
	// integers, so tmdb://603 is a valid id for a film and for an
	// unrelated series at the same time. Without this prefix the first
	// real build merged shows into films and lost about 24,000 titles:
	// 163,953 came out where over 190,000 was expected, and the missing
	// ones were almost exactly the show count.
	kind := it.Type
	if kind == "" {
		kind = "?"
	}
	if id := it.ExternalID("tmdb"); id != "" {
		return kind + "|tmdb:" + id
	}
	if id := it.ExternalID("imdb"); id != "" {
		return kind + "|imdb:" + id
	}
	if id := it.ExternalID("tvdb"); id != "" {
		return kind + "|tvdb:" + id
	}
	t := strings.ToLower(strings.TrimSpace(it.Title))
	t = strings.Join(strings.Fields(t), " ")
	if t != "" && it.Year > 0 {
		return kind + "|t:" + t + ":" + strconv.Itoa(it.Year)
	}
	return "u:" + strconv.Itoa(src) + ":" + it.RatingKey
}

// derive turns the raw per-section listings into one tree, dropping
// what a higher-priority server already has.
//
// Deduplication happens **between** servers only, never within one. A
// server's own section layout is its owner's curation and second
// guessing it does real damage: collapsing titles inside one server
// emptied exactly the sections that exist to hold a different copy of
// the same film. Measured on the real library, "Movies - Kids" fell from
// 1,636 titles to 26, "Movies - 3D" from 900 to 66 and "Movies - 4K-dv"
// from 5,601 to 1,078, because the ordinary copy in "Movies" is usually
// the larger file and won. The 3D version losing to the 2D one is not a
// tidier library, it is a broken one.
//
// So each server contributes everything it has, in its own sections, and
// a later server contributes only titles that no earlier server holds
// anywhere. That is the merge that was actually asked for: it removes
// the 76,068 films the two servers share, and touches nothing else.
func (c *catalogue) derive() {
	// Sources are processed in order so that an earlier one claims a
	// title before a later one can.
	maxSrc := 0
	for ss := range c.raw {
		if ss.src > maxSrc {
			maxSrc = ss.src
		}
	}

	bySection := make(map[string][]candidate, len(c.sections))
	claimed := make(map[string]bool, 200000)
	// where each claimed title ended up, so a later server's copy of it
	// can be attached as a fallback. A title can sit in several sections
	// of the winning server, and each of those placements wants the
	// fallback.
	type placement struct {
		section string
		idx     int
	}
	placed := make(map[string][]placement, 200000)
	spare := make(map[string][]altCopy)
	spareDirs := make(map[string][]altBranch)

	for src := 0; src <= maxSrc; src++ {
		// Keys this source adds. They are folded into claimed only after
		// the whole source is done, so a film this server keeps in three
		// sections stays in all three.
		mine := make(map[string]bool)
		for ss, items := range c.raw {
			if ss.src != src {
				continue
			}
			m := c.meta[ss]
			for i := range items {
				k := dedupKey(items[i], ss.src)
				// Mirrored, a copy the primary also has is a fallback and
				// nothing more. What the primary does not have either
				// falls away entirely, or goes to a section of this
				// server's own, depending on extraSections.
				keepAside := claimed[k] ||
					(mirrorPrimary && src > 0 && !extraSections)
				if keepAside {
					// An earlier server already shows this title. Keep
					// this copy aside as something to read from when
					// that server stops answering.
					if p := primaryPart(items[i]); p != nil {
						spare[k] = append(spare[k], altCopy{src: ss.src, part: *p})
					} else {
						// A show. Nothing to match on, because there is no
						// file: this server simply also knows the series,
						// which is enough to list its seasons from.
						spareDirs[k] = append(spareDirs[k], altBranch{
							src:        ss.src,
							sectionKey: ss.key,
							ratingKey:  items[i].RatingKey,
						})
					}
					continue
				}
				mine[k] = true
				name := m.name
				if src > 0 && mirrorPrimary && extraSections {
					name = extraName(name, c.srcName(src))
				}
				bySection[name] = append(bySection[name], candidate{
					src:        ss.src,
					sectionKey: ss.key,
					section:    name,
					item:       items[i],
					part:       primaryPart(items[i]),
				})
				placed[k] = append(placed[k], placement{name, len(bySection[name]) - 1})
			}
		}
		for k := range mine {
			claimed[k] = true
		}
	}

	// Attach the fallbacks, but only where the file is the same length
	// and the same container. See altCopy for why that is not optional.
	for k, copies := range spare {
		for _, p := range placed[k] {
			cd := &bySection[p.section][p.idx]
			if cd.part == nil {
				continue
			}
			for _, a := range copies {
				if a.part.Size == cd.part.Size && a.part.Container == cd.part.Container {
					cd.alts = append(cd.alts, a)
				}
			}
		}
	}

	// Attach the show fallbacks. No matching to do: another server that
	// knows the same series can list its seasons, and a short season list
	// beats a directory that never opens.
	for k, branches := range spareDirs {
		for _, p := range placed[k] {
			cd := &bySection[p.section][p.idx]
			if cd.part != nil {
				continue
			}
			cd.altDirs = append(cd.altDirs, branches...)
		}
	}

	// A fallback server's section that added nothing of its own must not
	// appear at all. Most of them add almost nothing: measured against
	// the real pair, "Movies - Remux" contributes 18 titles the primary
	// lacks and "TV Shows - Remux" two, because the primary already holds
	// those films in another form. An empty library is worse than a
	// missing one, since Plex will happily scan it forever.
	if len(c.extra) > 0 {
		// A fresh slice, not c.sections[:0]. A refresh derives into a
		// catalogue that shares this backing array with the one being
		// served, and pruning in place would rewrite the live root
		// listing underneath whoever is reading it.
		kept := make([]sectionRef, 0, len(c.sections))
		for _, s := range c.sections {
			if c.extra[s.name] && len(bySection[s.name]) == 0 {
				continue
			}
			kept = append(kept, s)
		}
		c.sections = kept
	}

	c.listings = make(map[string]*dirListing, len(bySection))
	for name, cands := range bySection {
		// Sorting keeps the exposed tree stable across rebuilds, which
		// matters because a mount above caches directory listings for
		// days and reshuffling them would look like churn.
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].item.Title != cands[j].item.Title {
				return cands[i].item.Title < cands[j].item.Title
			}
			return cands[i].item.RatingKey < cands[j].item.RatingKey
		})
		items := make([]plex.Item, 0, len(cands))
		for _, cd := range cands {
			items = append(items, cd.item)
		}
		entries := buildEntries(items)
		for i := range entries {
			entries[i].src = cands[i].src
			entries[i].sectionKey = cands[i].sectionKey
			entries[i].alts = cands[i].alts
			entries[i].altDirs = cands[i].altDirs
		}
		c.listings[name] = listingFrom(entries)
	}
	c.builtAt = time.Now()
}

// srcName names a source for display, falling back to its index if the
// catalogue was built without names (only tests do that).
func (c *catalogue) srcName(src int) string {
	if src < len(c.srcNames) && c.srcNames[src] != "" {
		return c.srcNames[src]
	}
	return "server" + strconv.Itoa(src)
}

// sectionList returns the merged root listing.
func (c *catalogue) sectionList() *dirListing {
	entries := make([]entry, 0, len(c.sections))
	for _, s := range c.sections {
		entries = append(entries, entry{name: s.name, item: plex.Item{Title: s.name}})
	}
	return listingFrom(entries)
}

// buildCatalogue lists every section of every source and deduplicates.
//
// This is expensive: roughly 280,000 records and several minutes of API
// calls. It runs once at startup and then only for sections Plex reports
// as changed, so the steady-state cost is one small poll per interval.
func (fs *FS) buildCatalogue(ctx context.Context) (*catalogue, error) {
	c := &catalogue{
		raw:  make(map[srcSection][]plex.Item),
		meta: make(map[srcSection]sectionRef),
	}
	for _, s := range fs.sources {
		c.srcNames = append(c.srcNames, s.Name)
	}

	// What the previous attempt managed to list, or nil on the first one.
	// A full build takes about half an hour against libraries this size
	// and the primary drops out several times a day, so an attempt that
	// starts clean and dies two thirds of the way through is the ordinary
	// case rather than the rare one. Reusing what it did get is what
	// makes the first catalogue reachable at all; without it, every
	// outage threw away the whole half hour and started over.
	prev := fs.lastAttempt()

	seen := make(map[string]bool)
	var firstErr error
	missing := 0

	// expose registers one section of one source as a root directory.
	expose := func(si int, sec sectionRef) {
		exposed := sec.name
		isExtra := si > 0 && mirrorPrimary && extraSections
		if isExtra {
			exposed = extraName(sec.name, c.srcName(si))
		}
		// Mirrored without extras, only the primary's sections become
		// directories: a fallback server's own would show up empty,
		// since nothing of its own is ever placed in them.
		if seen[exposed] || (si > 0 && mirrorPrimary && !extraSections) {
			return
		}
		seen[exposed] = true
		c.sections = append(c.sections, sectionRef{name: exposed, typ: sec.typ})
		if isExtra {
			if c.extra == nil {
				c.extra = make(map[string]bool)
			}
			c.extra[exposed] = true
		}
	}

	// carry replays everything the previous attempt held for one source,
	// for when that source cannot even be asked what its sections are.
	// The item slices are shared, not copied: they are never written to
	// after a build, so this costs nothing but a pointer.
	carry := func(si int) int {
		n := 0
		for _, ss := range prev.sectionOrder() {
			if ss.src != si {
				continue
			}
			items, ok := prev.rawOf(ss)
			if !ok {
				continue
			}
			m := prev.meta[ss]
			c.meta[ss] = m
			c.raw[ss] = items
			c.order = append(c.order, ss)
			expose(si, m)
			n++
		}
		return n
	}

	for si, s := range fs.sources {
		sections, err := s.Client.ListSections(ctx)
		if err != nil {
			// One server being down must not stop the other from being
			// exposed. This provider drops out for ten to twenty-five
			// minutes several times a day; refusing to build would mean
			// the whole mount is empty for that whole window.
			log.Printf("flexdav: %s: cannot list sections: %v", s.Name, err)
			if firstErr == nil {
				firstErr = err
			}
			if n := carry(si); n > 0 {
				c.carried += n
				log.Printf("flexdav: %s: kept the %d section(s) the previous attempt listed", s.Name, n)
			} else {
				missing++
			}
			continue
		}
		sections = fs.filterSections(sections)
		for _, sec := range sections {
			ref := sectionRef{name: sanitize(sec.Title), typ: sec.Type}
			ss := srcSection{src: si, key: sec.Key}
			items, err := s.Client.ListSectionAll(ctx, sec.Key)
			if err != nil {
				log.Printf("flexdav: %s: cannot list %q: %v", s.Name, sec.Title, err)
				if firstErr == nil {
					firstErr = err
				}
				old, ok := prev.rawOf(ss)
				if !ok {
					// Nothing to stand in with, so leave the section out
					// entirely rather than exposing it empty: an empty
					// library is worse than a missing one, since Plex
					// will scan it and record the deletions.
					missing++
					continue
				}
				items = old
				c.carried++
				log.Printf("flexdav: %s: kept the previous listing of %q, %d titles",
					s.Name, sec.Title, len(items))
			}
			c.meta[ss] = ref
			c.raw[ss] = items
			c.order = append(c.order, ss)
			expose(si, ref)
		}
	}
	if len(c.raw) == 0 {
		return nil, firstErr
	}
	// Only a section with no data at all makes this partial. One that
	// failed but had a previous listing to fall back on is stale by at
	// most one build, which is the same staleness the five-minute
	// refresh already accepts.
	c.partial = missing > 0
	c.derive()
	fs.rememberAttempt(c)
	return c, nil
}

// rawOf reads one section's items, tolerating a nil catalogue so that
// the first build needs no special case.
func (c *catalogue) rawOf(ss srcSection) ([]plex.Item, bool) {
	if c == nil {
		return nil, false
	}
	items, ok := c.raw[ss]
	return items, ok
}

// sectionOrder is the listing order, nil-tolerant for the same reason.
func (c *catalogue) sectionOrder() []srcSection {
	if c == nil {
		return nil
	}
	return c.order
}

// catalogueState guards the built catalogue and the build itself.
type catalogueState struct {
	mu    sync.RWMutex
	cur   *catalogue
	ready chan struct{} // closed once the first build lands
	once  sync.Once
}

func newCatalogueState() *catalogueState {
	return &catalogueState{ready: make(chan struct{})}
}

func (s *catalogueState) set(c *catalogue) {
	s.mu.Lock()
	s.cur = c
	s.mu.Unlock()
	s.once.Do(func() { close(s.ready) })
}

func (s *catalogueState) get() *catalogue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// wait blocks until a catalogue exists or the context ends.
//
// The first build takes minutes, and a request arriving during it has
// nothing useful to be told. Blocking is better than returning an empty
// tree: an empty tree is what a scanner would interpret as "everything
// was deleted".
func (s *catalogueState) wait(ctx context.Context) (*catalogue, error) {
	if c := s.get(); c != nil {
		return c, nil
	}
	select {
	case <-s.ready:
		return s.get(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
