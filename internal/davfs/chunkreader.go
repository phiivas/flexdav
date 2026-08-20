package davfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/phiivas/flexdav/internal/plex"
)

// readSource is one server's copy of the file being read. The first is
// the copy the catalogue chose; the rest are fallbacks for when it stops
// answering. See newChunkReader.
type readSource struct {
	name   string
	client *plex.Client
	part   plex.Part
}

// chunkReader reads a media Part through several concurrent ranged GETs
// and hands the bytes back in order.
//
// Do not trust any single measurement against this CDN. Two runs of the
// same parallel-curl test, hours apart on 2026-08-07:
//
//	streams    morning    night
//	      1     37-47      76.9
//	      4      66.7      25.3
//	      8      48.3      91.8
//	     16      18.5      65.1
//	     32         -      95.3
//
// The morning run looked like a clean peak at four streams and was read
// that way. The night run has a single stream beating four. The provider
// varies by more than the thing being measured, so the ranking was noise.
// Four is kept because it is unrefuted and matches Reaparr, not because
// it was ever demonstrated to be optimal.
//
// What did hold up: with one reader the bridge delivers 66.9 MB/s against
// 76.9 direct, so this pipeline costs about 13%. The mount on top was the
// expensive layer, not this.
//
// Chunk sizes ramp from small to large after every restart, so a seek
// delivers its first bytes quickly instead of waiting on a full-size
// chunk, while sustained reads still run at full width.
//
// A chunkReader is used by exactly one goroutine at a time for reading,
// matching how os.File is used, but its fetches run concurrently.
type chunkReader struct {
	srcs []readSource
	// cur is which source is serving right now. Chunks are fetched
	// concurrently, so this is atomic: whichever goroutine runs out of
	// retries first moves the rest on to the next server.
	cur      atomic.Int32
	size     int64
	workers  int
	maxChunk int64

	ctx         context.Context
	cancel      context.CancelFunc
	queue       []*chunkFuture // in ascending offset order
	next        int64          // next byte offset to schedule
	pos         int64          // current read position
	step        int            // chunks scheduled since the last restart
	servedBytes int64          // bytes consumed since the last restart
}

type chunkFuture struct {
	start int64
	size  int64
	done  chan struct{}
	data  []byte
	err   error
}

// newChunkReader builds the read pipeline over one or more copies of the
// same file.
//
// Fallbacks exist because the primary provider drops out for ten to
// twenty-five minutes at a time, several times a day, and during those
// windows an open simply hangs: measured 2026-08-08, thirty seconds
// without a byte and without an error.
//
// Every source here holds a file of the same byte length, which the
// catalogue guarantees. That constraint is not fussiness. The size is
// published when the directory is listed and the client remembers it, so
// serving a different length mid-stream breaks seeking and truncates the
// end. Of the 76,068 films both servers hold, 18,549 match exactly, and
// those are the ones that can be handed over safely.
//
// The pipeline geometry stays the primary's even after a switch. The two
// providers want different widths, but sorting that out mid-file is not
// worth it: during an outage the question is whether the film plays at
// all, not how fast it arrives.
func newChunkReader(srcs []readSource, workers int, maxChunk int64) *chunkReader {
	if workers < 1 {
		workers = 1
	}
	if maxChunk < firstChunkSize {
		maxChunk = firstChunkSize
	}
	r := &chunkReader{srcs: srcs, workers: workers, maxChunk: maxChunk}
	if len(srcs) > 0 {
		r.size = srcs[0].part.Size
	}
	return r
}

// source returns the copy currently being read.
func (r *chunkReader) source() readSource { return r.srcs[r.cur.Load()] }

// failOver moves to the next copy, if there is one, and reports whether
// it did. Called from a fetch goroutine that has run out of retries.
func (r *chunkReader) failOver(from int32) bool {
	if int(from)+1 >= len(r.srcs) {
		return false
	}
	// Only the first goroutine to notice advances; the others find the
	// index already moved and simply use it.
	if r.cur.CompareAndSwap(from, from+1) {
		log.Printf("flexdav: %s is not answering, switching to %s",
			r.srcs[from].name, r.srcs[from+1].name)
	}
	return true
}

// active reports whether a pipeline is running.
func (r *chunkReader) active() bool { return r.cancel != nil }

// seekTo repositions without tearing the pipeline down, when it can:
// either the read is simply continuing, or it lands inside the chunk at
// the head of the queue. Anything further away needs a restart.
func (r *chunkReader) seekTo(offset int64) bool {
	if !r.active() {
		return false
	}
	if offset == r.pos {
		return true
	}
	if len(r.queue) > 0 {
		h := r.queue[0]
		if offset >= h.start && offset < h.start+h.size {
			r.pos = offset
			return true
		}
	}
	return false
}

func (r *chunkReader) restart(offset int64) {
	r.stop()
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.pos, r.next, r.step, r.servedBytes = offset, offset, 0, 0
	r.queue = nil
	r.fill()
}

// stop cancels every in-flight fetch and drops the buffers.
func (r *chunkReader) stop() {
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.queue = nil
}

func (r *chunkReader) chunkSize() int64 {
	size := int64(firstChunkSize) << r.step
	if size > r.maxChunk || size <= 0 {
		size = r.maxChunk
	}
	return size
}

// width is how many chunks may be in flight. It opens up only as the
// reader proves it is actually consuming the file.
//
// This matters more than it looks. A library scanner opens a file, reads
// a few hundred KB of container header, seeks to the index at the end,
// and closes it again. Filling the queue to full width on the first read
// fetched about 15 MiB per open (1+2+4+8 MiB, since chunk sizes ramp),
// twice over because the seek to the end restarts the pipeline. Plex
// needed 320 KB per film and the bridge pulled 75 MB: 240x amplification,
// measured on 2026-08-07 while scanning a 900-film library.
//
// The threshold has to be small. An earlier version opened the width one
// chunk at a time, which starved playback: rclone issues a fresh ranged
// GET per VFS chunk, every GET restarts the pipeline, and a pipeline that
// starts one chunk wide moves one chunk per round trip. Throughput fell
// under 1 MB/s and Plex hung outright. One chunk of evidence is enough,
// so full width arrives after the first one.
func (r *chunkReader) width() int {
	if r.servedBytes >= firstChunkSize {
		return r.workers
	}
	return 1
}

// fill tops the pipeline back up to the current width.
func (r *chunkReader) fill() {
	for len(r.queue) < r.width() && r.next < r.size {
		size := r.chunkSize()
		if r.next+size > r.size {
			size = r.size - r.next
		}
		f := &chunkFuture{start: r.next, size: size, done: make(chan struct{})}
		r.queue = append(r.queue, f)
		r.next += size
		r.step++
		go r.fetch(r.ctx, f)
	}
}

func (r *chunkReader) read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if len(r.queue) == 0 {
		return 0, io.EOF
	}

	h := r.queue[0]
	select {
	case <-h.done:
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
	if h.err != nil {
		return 0, h.err
	}

	off := r.pos - h.start
	if off < 0 || off >= int64(len(h.data)) {
		return 0, fmt.Errorf("flexdav: read position %d outside chunk [%d,%d)",
			r.pos, h.start, h.start+h.size)
	}
	n := copy(p, h.data[off:])
	r.pos += int64(n)

	if r.pos >= h.start+h.size {
		r.queue = r.queue[1:]
		r.servedBytes += h.size
		r.fill()
	}
	return n, nil
}

// fetch retrieves one chunk, retrying on its own. Because failures are
// contained to a single chunk, a dropped connection costs at most
// maxChunk bytes of re-transfer rather than the whole file.
func (r *chunkReader) fetch(ctx context.Context, f *chunkFuture) {
	defer close(f.done)

	buf := make([]byte, f.size)
	for attempt := 0; ; {
		src := r.cur.Load()
		err := r.fetchOnce(ctx, f.start, buf)
		if err == nil {
			f.data = buf
			return
		}
		if ctx.Err() != nil {
			f.err = ctx.Err()
			return
		}
		attempt++
		// Be impatient while there is somewhere else to go, patient
		// once this is the last copy.
		limit := maxReadRetries
		if int(src)+1 < len(r.srcs) {
			limit = retriesBeforeFailover
		}
		if attempt > limit {
			// This copy is not coming back soon enough. If another
			// server holds the same file, hand over and start the
			// retry count again rather than failing the read.
			if r.failOver(src) {
				attempt = 0
				continue
			}
			f.err = err
			return
		}
		select {
		case <-ctx.Done():
			f.err = ctx.Err()
			return
		case <-time.After(backoff(attempt)):
		}
	}
}

// debugChunks turns on per-chunk timing. Reasoning about this pipeline
// from the outside produced three wrong diagnoses in one day, so the
// numbers come from inside it now.
var debugChunks = os.Getenv("PLEX_DEBUG_CHUNKS") != ""

func (r *chunkReader) fetchOnce(ctx context.Context, start int64, buf []byte) error {
	var t0, tHeader time.Time
	if debugChunks {
		t0 = time.Now()
	}
	end := start + int64(len(buf)) - 1
	src := r.source()
	resp, err := src.client.OpenPart(ctx, src.part, fmt.Sprintf("bytes=%d-%d", start, end))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if debugChunks {
		tHeader = time.Now()
		defer func() {
			total := time.Since(t0).Seconds()
			body := time.Since(tHeader).Seconds()
			mb := float64(len(buf)) / (1 << 20)
			log.Printf("chunk off=%d size=%.0fMiB wait=%.2fs body=%.2fs total=%.2fs %.1fMB/s",
				start, mb, tHeader.Sub(t0).Seconds(), body, total, mb/total)
		}()
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		// Range ignored, so discard up to where we wanted to start
		// rather than returning the wrong bytes.
		if start > 0 {
			if _, err := io.CopyN(io.Discard, resp.Body, start); err != nil {
				return err
			}
		}
	default:
		return errors.New("flexdav: upstream returned " + resp.Status)
	}

	_, err = io.ReadFull(resp.Body, buf)
	return err
}
