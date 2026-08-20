// Package plexlocal asks the local Plex server to rescan one directory.
//
// Plex's own "run a partial scan when changes are detected" relies on
// filesystem notifications, which FUSE does not provide. Left alone, a
// mounted remote library can gain a hundred episodes and Plex will not
// notice for days. The bridge already knows the moment a section changes,
// so it tells Plex directly.
//
// Going through Plex's API rather than the Plex Media Scanner binary is
// deliberate: the work then belongs to the server, shows up in its
// activity list, and can be watched or cancelled from the interface like
// any other scan.
package plexlocal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

type Client struct {
	base  string
	token string
	http  *http.Client

	mu       sync.Mutex
	sections map[string]Section // keyed by the last component of the path
}

// Section is one Plex library and the directory it watches.
type Section struct {
	ID    string
	Title string
	Path  string
}

// New returns nil when either argument is empty, which is the signal that
// no local Plex is configured and the caller should skip notifying.
func New(baseURL, token string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	token = strings.TrimSpace(token)
	if baseURL == "" || token == "" {
		return nil
	}
	return &Client{
		base:  baseURL,
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

type sectionsResponse struct {
	MediaContainer struct {
		Directory []struct {
			Key      string `json:"key"`
			Title    string `json:"title"`
			Location []struct {
				Path string `json:"path"`
			} `json:"Location"`
		} `json:"Directory"`
	} `json:"MediaContainer"`
}

// loadSections maps each library's directory name to the library itself.
// Matching on the directory name rather than a configured prefix means
// libraries can be added or moved in Plex without touching the bridge.
func (c *Client) loadSections(ctx context.Context) error {
	body, err := c.get(ctx, "/library/sections")
	if err != nil {
		return err
	}
	var resp sectionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("plexlocal: %w", err)
	}
	found := make(map[string]Section)
	for _, d := range resp.MediaContainer.Directory {
		for _, l := range d.Location {
			if l.Path == "" {
				continue
			}
			found[path.Base(strings.TrimRight(l.Path, "/"))] = Section{
				ID: d.Key, Title: d.Title, Path: l.Path,
			}
		}
	}
	c.mu.Lock()
	c.sections = found
	c.mu.Unlock()
	return nil
}

func (c *Client) lookup(name string) (Section, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sections[name]
	return s, ok
}

// ScanSections asks Plex to rescan the libraries whose directories carry
// these names. Names it does not recognise are reported but do not stop
// the rest: a remote server exposes sections the local Plex has no
// library for, and that is the normal case, not an error.
func (c *Client) ScanSections(ctx context.Context, names []string) error {
	if c == nil || len(names) == 0 {
		return nil
	}
	c.mu.Lock()
	empty := len(c.sections) == 0
	c.mu.Unlock()
	if empty {
		if err := c.loadSections(ctx); err != nil {
			return err
		}
	}

	var missing, failed []string
	for _, n := range names {
		s, ok := c.lookup(n)
		if !ok {
			// A library may have been added since the last load.
			if err := c.loadSections(ctx); err == nil {
				s, ok = c.lookup(n)
			}
		}
		if !ok {
			missing = append(missing, n)
			continue
		}
		if err := c.refresh(ctx, s); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", n, err))
		}
	}

	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "no local library for "+strings.Join(missing, ", "))
	}
	if len(failed) > 0 {
		parts = append(parts, "scan failed for "+strings.Join(failed, "; "))
	}
	if len(parts) > 0 {
		return errors.New("plexlocal: " + strings.Join(parts, "; "))
	}
	return nil
}

// refresh triggers a partial scan of one directory rather than the whole
// library. Over a remote mount the difference is not cosmetic: a full
// scan of a large section costs hours.
func (c *Client) refresh(ctx context.Context, s Section) error {
	q := url.Values{}
	q.Set("path", s.Path)
	_, err := c.get(ctx, "/library/sections/"+s.ID+"/refresh?"+q.Encode())
	return err
}

func (c *Client) get(ctx context.Context, p string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Plex-Token", c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.redact(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plexlocal: %s", resp.Status)
	}
	return body, nil
}

// redact keeps the token out of error strings. Go puts the whole URL in
// *url.Error, and these errors end up in the container log.
func (c *Client) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ReplaceAll(err.Error(), c.token, "***")
	return errors.New(msg)
}
