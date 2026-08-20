package plex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

const secret = "SuPerSecretPlexToken12345"

// Transport failures carry the full request URL, and the token rides in
// the query string. Those errors get logged and handed back to WebDAV
// clients, so the credential must never survive in the message.
func TestTransportErrorsRedactToken(t *testing.T) {
	// Port 1 on the loopback refuses connections, producing a *url.Error
	// that quotes the URL.
	c := NewClient("http://127.0.0.1:1", secret)

	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("token leaked into error: %s", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("expected redaction marker, got: %s", err)
	}
}

func TestOpenPartErrorsRedactToken(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", secret)

	_, err := c.OpenPart(context.Background(), Part{Key: "/library/parts/1/2/file.mkv"}, "bytes=0-")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("token leaked into error: %s", err)
	}
}

// PartURL legitimately contains the token; this pins that it is escaped
// so a token with URL-significant characters cannot break the query.
func TestPartURLEscapesToken(t *testing.T) {
	c := NewClient("https://example.invalid", "tok&en=weird value")
	got := c.PartURL(Part{Key: "/library/parts/1/2/file.mkv"})
	if strings.Contains(got, "tok&en=weird value") {
		t.Errorf("token was not escaped: %s", got)
	}
	if !strings.Contains(got, "tok%26en%3Dweird+value") {
		t.Errorf("unexpected escaping: %s", got)
	}
}

// One record in a large section can break Plex's own serialiser: any
// window containing it returns 500 instantly, every time. Retrying
// cannot help, and giving up drops the whole section, which is exactly
// what silently removed a 99,426 film library from the first merged
// catalogue. The listing has to narrow down onto the bad record, step
// over it, and finish.
func TestListingSkipsAnUnlistableRecord(t *testing.T) {
	const total, poison = 2500, 1704

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		start, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
		size, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Size"))
		if start <= poison && poison < start+size {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		end := start + size
		if end > total {
			end = total
		}
		items := make([]map[string]any, 0, end-start)
		for i := start; i < end && i < total; i++ {
			items = append(items, map[string]any{
				"ratingKey": strconv.Itoa(i),
				"title":     "Film " + strconv.Itoa(i),
				"type":      "movie",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"MediaContainer": map[string]any{"Metadata": items},
		})
	}))
	defer srv.Close()

	items, err := NewClient(srv.URL, "tok").ListSectionAll(context.Background(), "1")
	if err != nil {
		t.Fatalf("ListSectionAll: %v", err)
	}
	if len(items) != total-1 {
		t.Errorf("got %d items, want %d (everything but the one bad record)", len(items), total-1)
	}
	for _, it := range items {
		if it.RatingKey == strconv.Itoa(poison) {
			t.Errorf("the unlistable record came back anyway")
		}
	}
	if requests > 60 {
		t.Errorf("took %d requests; narrowing should cost a handful, not a scan", requests)
	}
}

// A status that is not the server refusing one window must still fail
// the listing rather than being narrowed away: a 401 means the token is
// wrong, and quietly returning a short library would hide that.
func TestListingDoesNotSkipOnAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "tok").ListSectionAll(context.Background(), "1"); err == nil {
		t.Error("a 401 should fail the listing")
	}
}
