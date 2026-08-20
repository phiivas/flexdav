// Package rclonerc talks to an rclone mount's remote-control API.
//
// It exists for one job: telling the mount above this bridge to drop its
// cached view of a directory. Without it, a long --dir-cache-time is a
// trade between "new episodes appear promptly" and "listing a 99k-item
// section every few minutes". With it there is no trade: the cache can be
// held for days, because the bridge knows exactly when a section changed
// and says so.
package rclonerc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	base string
	user string
	pass string
	http *http.Client
}

// New returns nil when addr is empty, which is the signal that no mount
// is listening and the caller should simply skip notifying.
//
// Credentials may be carried in the address as http://user:pass@host:port.
// They are worth setting: rclone's rc API can do far more than forget a
// directory, and on a Docker host the address has to be reachable from a
// container, which means it is reachable from every other container too.
func New(addr string) *Client {
	addr = strings.TrimSpace(strings.TrimRight(addr, "/"))
	if addr == "" {
		return nil
	}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	c := &Client{base: addr, http: &http.Client{Timeout: 15 * time.Second}}
	if u, err := url.Parse(addr); err == nil && u.User != nil {
		c.user = u.User.Username()
		c.pass, _ = u.User.Password()
		u.User = nil
		c.base = strings.TrimRight(u.String(), "/")
	}
	return c
}

// Forget makes the mount discard its cached listing for the named
// directories, which are paths relative to the remote root. Failures are
// returned but are not worth treating as fatal: the worst case is that a
// new episode waits for the normal cache expiry.
func (c *Client) Forget(ctx context.Context, dirs []string) error {
	if c == nil || len(dirs) == 0 {
		return nil
	}
	var failed []string
	for _, d := range dirs {
		if err := c.post(ctx, "vfs/forget", map[string]string{"dir": d}); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", d, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("rclonerc: could not forget %s", strings.Join(failed, "; "))
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, params map[string]string) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Addr is the address with any credentials removed, safe to log.
// Logging the configured URL directly writes the password into the
// container log in plain text, which is exactly what happened the first
// time this was wired up.
func (c *Client) Addr() string {
	if c == nil {
		return ""
	}
	return c.base
}
