package plexlocal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakePlex answers /library/sections and records refresh calls.
type fakePlex struct {
	srv       *httptest.Server
	refreshes chan string
	loads     atomic.Int32
	body      string
}

const twoSections = `{"MediaContainer":{"Directory":[
  {"key":"17","title":"Movies","Location":[{"path":"/data/media/movies-hd"}]},
  {"key":"18","title":"Movies - 3D","Location":[{"path":"/data/remote/Movies - 3D"}]}
]}}`

func newFake(t *testing.T, body string) *fakePlex {
	t.Helper()
	f := &fakePlex{refreshes: make(chan string, 8), body: body}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") == "" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/library/sections":
			f.loads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.body))
		case strings.HasSuffix(r.URL.Path, "/refresh"):
			f.refreshes <- r.URL.Path + "?path=" + r.URL.Query().Get("path")
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestScanRefreshesOnlyTheChangedDirectory(t *testing.T) {
	f := newFake(t, twoSections)
	c := New(f.srv.URL, "tok")

	if err := c.ScanSections(context.Background(), []string{"Movies - 3D"}); err != nil {
		t.Fatalf("ScanSections: %v", err)
	}
	select {
	case got := <-f.refreshes:
		want := "/library/sections/18/refresh?path=/data/remote/Movies - 3D"
		if got != want {
			t.Errorf("refreshed %q, want %q", got, want)
		}
	default:
		t.Fatal("no refresh was requested")
	}
	if len(f.refreshes) != 0 {
		t.Errorf("%d extra refreshes, want none", len(f.refreshes))
	}
}

// The remote servers expose sections the local Plex has no library for.
// That is normal, and it must not stop the sections that do map.
func TestUnknownSectionDoesNotBlockTheKnownOnes(t *testing.T) {
	f := newFake(t, twoSections)
	c := New(f.srv.URL, "tok")

	err := c.ScanSections(context.Background(), []string{"Courses", "Movies - 3D"})
	if err == nil || !strings.Contains(err.Error(), "Courses") {
		t.Errorf("error = %v, want it to name the unmapped section", err)
	}
	select {
	case got := <-f.refreshes:
		if !strings.Contains(got, "/18/refresh") {
			t.Errorf("refreshed %q, want section 18", got)
		}
	default:
		t.Fatal("the mapped section was never refreshed")
	}
}

// A library added in Plex after startup must be picked up rather than
// requiring a restart of the bridge.
func TestSectionsAreReloadedWhenANameIsUnknown(t *testing.T) {
	f := newFake(t, `{"MediaContainer":{"Directory":[
	  {"key":"17","title":"Movies","Location":[{"path":"/data/media/movies-hd"}]}
	]}}`)
	c := New(f.srv.URL, "tok")

	_ = c.ScanSections(context.Background(), []string{"Movies"})
	before := f.loads.Load()

	f.body = twoSections // the user adds another library
	if err := c.ScanSections(context.Background(), []string{"Movies - 3D"}); err != nil {
		t.Fatalf("ScanSections after the library was added: %v", err)
	}
	if f.loads.Load() <= before {
		t.Error("sections were not reloaded when an unknown name turned up")
	}
	select {
	case <-f.refreshes:
	default:
		t.Fatal("the newly added library was never refreshed")
	}
}

func TestNoConfigIsSafeToCall(t *testing.T) {
	if c := New("", "tok"); c != nil {
		t.Error("New with no URL should return nil")
	}
	if c := New("http://x", ""); c != nil {
		t.Error("New with no token should return nil")
	}
	var c *Client
	if err := c.ScanSections(context.Background(), []string{"Movies"}); err != nil {
		t.Errorf("ScanSections on a nil client: %v, want nil", err)
	}
}

// Go puts the whole URL into *url.Error, and these errors are logged.
func TestErrorsDoNotLeakTheToken(t *testing.T) {
	c := New("http://127.0.0.1:1", "super-secret-token")
	err := c.ScanSections(context.Background(), []string{"Movies"})
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("error leaks the token: %v", err)
	}
}
