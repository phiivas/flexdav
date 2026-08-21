package davfs

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/phiivas/flexdav/internal/plex"
)

// bigPart serves a part large enough that the pipeline would want several
// chunks, and counts how many ranged GETs it is asked for.
type bigPart struct {
	size int64
	gets atomic.Int32
	srv  *httptest.Server

	// A restart cancels the previous pipeline, but a fetch goroutine it
	// has already launched can still reach the server afterwards, so
	// counting every GET makes assertions about a seek racy. Tests that
	// care set mark to the offset they seek to; past counts only the
	// requests that belong to the new pipeline.
	mark atomic.Int64
	past atomic.Int32
}

func newBigPart(t *testing.T, size int64) *bigPart {
	t.Helper()
	b := &bigPart{size: size}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.gets.Add(1)
		start, end := int64(0), b.size-1
		if rh := r.Header.Get("Range"); strings.HasPrefix(rh, "bytes=") {
			lo, hi, _ := strings.Cut(strings.TrimPrefix(rh, "bytes="), "-")
			if v, err := strconv.ParseInt(lo, 10, 64); err == nil {
				start = v
			}
			if v, err := strconv.ParseInt(hi, 10, 64); err == nil && v < end {
				end = v
			}
		}
		if m := b.mark.Load(); m > 0 && start >= m {
			b.past.Add(1)
		}
		n := end - start + 1
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, b.size))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.CopyN(w, zeroes{}, n)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func (b *bigPart) source(name string) readSource {
	return readSource{
		name:   name,
		client: plex.NewClient(b.srv.URL, "test-token"),
		part:   plex.Part{Key: "/library/parts/1/1/file.mkv", Size: b.size},
	}
}

func (b *bigPart) reader(streams int) *chunkReader {
	return newChunkReader([]readSource{b.source("primary")}, streams, defaultMaxChunk)
}

// A library scanner opens a file, reads a little, and closes it. That must
// not drag the whole prefetch pipeline in behind it: filling the queue to
// full width on the first read cost 15 MiB per open and made scanning a
// remote library unusable.
func TestSmallReadDoesNotFillThePipeline(t *testing.T) {
	b := newBigPart(t, 8<<30)
	r := b.reader(defaultStreams)
	r.restart(0)
	defer r.stop()

	buf := make([]byte, 320<<10)
	if _, err := r.read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}

	if got := b.gets.Load(); got != 1 {
		t.Errorf("a %d KB read issued %d upstream GETs, want 1", len(buf)>>10, got)
	}
}

// The ramp must still open up, or sustained reads would crawl along one
// chunk at a time and lose all the benefit of the parallel pipeline.
func TestSustainedReadReachesFullWidth(t *testing.T) {
	b := newBigPart(t, 8<<30)
	r := b.reader(defaultStreams)
	r.restart(0)
	defer r.stop()

	// Consume the first chunk so the width can open.
	buf := make([]byte, 1<<20)
	for read := 0; read < 2<<20; {
		n, err := r.read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		read += n
	}

	if got := r.width(); got != defaultStreams {
		t.Errorf("width after a sustained read is %d, want %d", got, defaultStreams)
	}
	if got := len(r.queue); got != defaultStreams {
		t.Errorf("%d chunks in flight after a sustained read, want %d", got, defaultStreams)
	}
}

// A seek restarts the pipeline, and the narrow start has to come back with
// it. Plex reads a header at the front and an index at the back of every
// file it scans, so the second access must be as cheap as the first.
func TestSeekResetsTheRamp(t *testing.T) {
	b := newBigPart(t, 8<<30)
	r := b.reader(defaultStreams)
	r.restart(0)
	defer r.stop()

	buf := make([]byte, 1<<20)
	for read := 0; read < 2<<20; {
		n, err := r.read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		read += n
	}

	seek := b.size - (64 << 20) // near the end, as an index read does
	b.mark.Store(seek)
	r.restart(seek)
	if _, err := r.read(buf[:320<<10]); err != nil {
		t.Fatalf("read after seek: %v", err)
	}

	if got := b.past.Load(); got != 1 {
		t.Errorf("a small read after seeking issued %d upstream GETs, want 1", got)
	}
}

// A shared server can be unreachable for many minutes at a time.
// When another server holds the identical
// file, a read must carry on from it instead of hanging.
func TestReadFallsOverToTheOtherServer(t *testing.T) {
	const size = 8 << 20
	dead := newBigPart(t, size)
	dead.srv.Close() // refuses connections, like a provider that has gone
	alive := newBigPart(t, size)

	r := newChunkReader([]readSource{dead.source("dead"), alive.source("alive")}, 4, defaultMaxChunk)
	defer r.stop()
	r.restart(0)

	buf := make([]byte, 64<<10)
	n, err := r.read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n == 0 {
		t.Fatal("read returned no bytes")
	}
	if alive.gets.Load() == 0 {
		t.Error("the surviving server was never asked")
	}
	if got := r.cur.Load(); got != 1 {
		t.Errorf("reader is still on source %d, want 1", got)
	}
}

// With no fallback configured the failure has to surface. Silently
// returning short reads would look to a player like the file ended.
func TestReadWithNoFallbackStillFails(t *testing.T) {
	dead := newBigPart(t, 8<<20)
	dead.srv.Close()

	r := newChunkReader([]readSource{dead.source("dead")}, 1, defaultMaxChunk)
	defer r.stop()
	r.restart(0)

	if _, err := r.read(make([]byte, 4096)); err == nil {
		t.Error("expected an error when the only server is gone")
	}
}

// Once handed over, the read stays on the fallback. Going back to a
// server that is still down would stall on every single chunk.
func TestFailoverIsNotRepeatedPerChunk(t *testing.T) {
	const size = 8 << 20
	dead := newBigPart(t, size)
	dead.srv.Close()
	alive := newBigPart(t, size)

	r := newChunkReader([]readSource{dead.source("dead"), alive.source("alive")}, 1, defaultMaxChunk)
	defer r.stop()
	r.restart(0)

	buf := make([]byte, 256<<10)
	read := 0
	for read < size {
		n, err := r.read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read at %d: %v", read, err)
		}
		read += n
	}
	if read != size {
		t.Errorf("read %d bytes of %d", read, size)
	}
	if got := r.cur.Load(); got != 1 {
		t.Errorf("reader moved to source %d", got)
	}
}
