package rclonerc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForgetPostsOneRequestPerDirectory(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vfs/forget" {
			t.Errorf("posted to %q, want /vfs/forget", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var params map[string]string
		if err := json.Unmarshal(body, &params); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		got = append(got, params["dir"])
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if err := New(srv.URL).Forget(context.Background(), []string{"Movies - 4K", "TV Shows"}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if len(got) != 2 || got[0] != "Movies - 4K" || got[1] != "TV Shows" {
		t.Errorf("forgot %v, want [Movies - 4K TV Shows]", got)
	}
}

// No mount listening is the normal case for a bare container, not an
// error, so New must hand back something safe to call.
func TestNoAddressIsSafeToCall(t *testing.T) {
	if c := New("  "); c != nil {
		t.Fatalf("New(empty) returned %v, want nil", c)
	}
	var c *Client
	if err := c.Forget(context.Background(), []string{"Movies"}); err != nil {
		t.Errorf("Forget on a nil client: %v, want nil", err)
	}
}

// A mount that refuses the call must not take the bridge down with it.
// The cost of failure is only that new content waits for normal expiry.
func TestFailureIsReportedNotPanicked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "directory not found", http.StatusNotFound)
	}))
	defer srv.Close()

	err := New(srv.URL).Forget(context.Background(), []string{"Nope"})
	if err == nil {
		t.Fatal("Forget succeeded against a 404, want an error")
	}
	if !strings.Contains(err.Error(), "Nope") {
		t.Errorf("error %q does not name the directory that failed", err)
	}
}

func TestBareHostGetsAScheme(t *testing.T) {
	c := New("127.0.0.1:5572")
	if c == nil || !strings.HasPrefix(c.base, "http://") {
		t.Fatalf("New(%q) produced %+v, want an http:// base", "127.0.0.1:5572", c)
	}
}

// Credentials in the address must actually reach the wire, and must not
// leak into the base URL that gets logged.
func TestCredentialsAreSentAndStrippedFromBase(t *testing.T) {
	var sawUser, sawPass string
	var ok bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUser, sawPass, ok = r.BasicAuth()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	withCreds := strings.Replace(srv.URL, "http://", "http://bob:s3cret@", 1)
	c := New(withCreds)
	if strings.Contains(c.base, "s3cret") {
		t.Errorf("base URL %q still carries the password", c.base)
	}
	if err := c.Forget(context.Background(), []string{"Movies"}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if !ok || sawUser != "bob" || sawPass != "s3cret" {
		t.Errorf("server saw user=%q pass set=%v, want bob and true", sawUser, sawPass != "")
	}
}

// Addr is what gets logged, so it must never carry the password.
func TestAddrHidesCredentials(t *testing.T) {
	c := New("http://bob:s3cret@127.0.0.1:5572")
	if strings.Contains(c.Addr(), "s3cret") || strings.Contains(c.Addr(), "bob") {
		t.Errorf("Addr() = %q, still carries credentials", c.Addr())
	}
	if !strings.Contains(c.Addr(), "127.0.0.1:5572") {
		t.Errorf("Addr() = %q, lost the host", c.Addr())
	}
}
