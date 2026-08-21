// Package plex is a minimal read-only client for the parts of the Plex
// Media Server HTTP API this bridge needs: listing library sections,
// browsing items (shows/seasons/episodes/movies), and streaming a media
// Part with HTTP Range support so clients can seek.
package plex

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// metadataRetries and metadataBackoff govern listing requests only;
	// media reads have their own retry handling in the reader.
	metadataRetries = 3
	metadataBackoff = 500 * time.Millisecond

	pageSize = 1000
	// maxPages guards against a server that keeps returning full pages
	// forever; 1000 * 1000 items is far beyond any real library.
	// Narrowing a poisoned window consumes iterations of the same loop,
	// so this is well above what plain paging needs.
	maxPages = 5000
	// maxSkips bounds how many individually unlistable records a section
	// may have before the listing is called a failure rather than a gap.
	maxSkips = 50

	// dialTimeout bounds connection setup. Shared servers usually sit behind
	// links with round trips around 200ms, so a healthy dial is well
	// under a second; anything past this is an outage, not slowness.
	dialTimeout = 15 * time.Second
)

type Client struct {
	baseURL string // e.g. https://plex.example.com
	token   string
	http    *http.Client
}

// NewClient builds a client that forces HTTP/1.1. See NewClientHTTP2 for
// why that is not always the right choice.
func NewClient(baseURL, token string) *Client {
	return newClient(baseURL, token, false)
}

// NewClientHTTP2 allows HTTP/2. Which protocol is faster turns out to be
// a property of the server, not a universal answer, and the difference is
// not small. Measured with 32 MiB ranged reads and a fresh connection each
// time, one server carried about seven times as much over HTTP/2 as over
// HTTP/1.1. On another the opposite held: HTTP/2 multiplexed all four chunk
// fetches onto one TCP connection and made the read pipeline parallel in
// name only. So this is a per-server switch, and neither setting is a safe
// default for an unknown server. Measure.
func NewClientHTTP2(baseURL, token string) *Client {
	return newClient(baseURL, token, true)
}

func newClient(baseURL, token string, http2 bool) *Client {
	noKeepAlive := os.Getenv("PLEX_NO_KEEPALIVE") != ""
	// An empty non-nil TLSNextProto is the documented way to turn HTTP/2
	// off; leaving it nil lets Go negotiate h2 over ALPN.
	var nextProto map[string]func(string, *tls.Conn) http.RoundTripper
	if !http2 {
		nextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			// No global timeout: media reads are long-lived streams and
			// would be killed mid-transfer. Per-phase timeouts below
			// still bound connection setup and header latency.
			Transport: &http.Transport{
				// Without this net/http dials with a zero-value Dialer,
				// which has no timeout at all: a provider that swallows
				// packets rather than refusing them leaves every attempt
				// waiting on the kernel's SYN retries, about two minutes
				// each. Five of those in a row is ten minutes before the
				// reader gives up and moves to the other server, which is
				// far too late to be of any use to a player.
				DialContext: (&net.Dialer{
					Timeout:   dialTimeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
				// Deep pagination is slow: asking a 24k-show library for
				// items from offset 15000 regularly takes far longer
				// than a first page, and 30s here was cutting those off.
				ResponseHeaderTimeout: 120 * time.Second,
				ExpectContinueTimeout: 5 * time.Second,
				// net/http defaults to 4 KiB buffers. On a link with
				// ~200ms round trips that throttles a single media
				// stream badly; media reads are the whole workload here,
				// so give them room.
				ReadBufferSize:  512 << 10,
				WriteBufferSize: 64 << 10,
				TLSNextProto:    nextProto,
				// Some providers throttle a connection by how much it has
				// already delivered, so reuse is a penalty rather than a
				// saving: a reused connection measured about a third
				// slower than a fresh one. Off by default because a fresh TLS
				// handshake per chunk is real cost on a high-latency link.
				DisableKeepAlives: noKeepAlive,
			},
		},
	}
}

// BaseURL returns the configured server URL (used for diagnostics).
func (c *Client) BaseURL() string { return c.baseURL }

// redact strips the auth token out of an error before it can escape.
//
// The token travels as a query parameter, and net/http puts the full URL
// into *url.Error, so an unredacted transport failure would spill the
// credential into logs and into WebDAV error responses.
func (c *Client) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if c.token != "" {
		msg = strings.ReplaceAll(msg, c.token, "***")
		if esc := url.QueryEscape(c.token); esc != c.token {
			msg = strings.ReplaceAll(msg, esc, "***")
		}
	}
	return errors.New(msg)
}

// get issues a metadata request, retrying transient failures.
//
// Deep pagination against a large library is flaky: requests for items
// past offset ~15000 intermittently stall. Reaparr's own logs show the
// same endpoint failing and being retried, so a single attempt is not
// enough to list a big section reliably.
func (c *Client) get(ctx context.Context, path string, query url.Values, v any) error {
	var lastErr error
	for attempt := 0; attempt <= metadataRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(metadataBackoff << (attempt - 1)):
			}
		}
		err := c.getOnce(ctx, path, query, v)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
	}
	return lastErr
}

func (c *Client) getOnce(ctx context.Context, path string, query url.Values, v any) error {
	if query == nil {
		query = url.Values{}
	}
	query = cloneValues(query)
	query.Set("X-Plex-Token", c.token)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return c.redact(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &StatusError{Code: resp.StatusCode, Status: resp.Status, Path: path}
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// cloneValues copies the caller's query so retries never accumulate
// parameters onto the same map.
func cloneValues(in url.Values) url.Values {
	out := make(url.Values, len(in)+1)
	for k, v := range in {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// Ping verifies the base URL and token are usable.
func (c *Client) Ping(ctx context.Context) error {
	return c.get(ctx, "/identity", nil, nil)
}

// ListSections returns the top-level library sections.
func (c *Client) ListSections(ctx context.Context) ([]Directory, error) {
	var out mediaContainer[sectionsResponse]
	if err := c.get(ctx, "/library/sections", nil, &out); err != nil {
		return nil, err
	}
	return out.MediaContainer.Directory, nil
}

// ListSectionAll returns every top-level item (shows or movies) in a
// section, with external ids attached so callers can recognise the same
// title on a different server.
func (c *Client) ListSectionAll(ctx context.Context, sectionKey string) ([]Item, error) {
	return c.listPaged(ctx, fmt.Sprintf("/library/sections/%s/all", sectionKey), true)
}

// ListChildren returns the children of a show (seasons) or season (episodes).
func (c *Client) ListChildren(ctx context.Context, ratingKey string) ([]Item, error) {
	return c.listPaged(ctx, fmt.Sprintf("/library/metadata/%s/children", ratingKey), false)
}

// listPaged walks Plex's X-Plex-Container-Start/Size windowing.
//
// Termination is driven by a short page rather than by totalSize: that
// field is absent on several endpoints, and trusting it would stop
// after the first page whenever it reads back as zero.
func (c *Client) listPaged(ctx context.Context, path string, guids bool) ([]Item, error) {
	var all []Item
	start, size, skipped := 0, pageSize, 0

	for page := 0; page < maxPages; page++ {
		q := url.Values{
			"X-Plex-Container-Start": {strconv.Itoa(start)},
			"X-Plex-Container-Size":  {strconv.Itoa(size)},
		}
		if guids {
			q.Set("includeGuids", "1")
		}
		var out mediaContainer[itemsResponse]
		err := c.get(ctx, path, q, &out)
		if err != nil {
			var se *StatusError
			if !errors.As(err, &se) || se.Code < 500 || se.Code > 599 {
				return nil, err
			}
			// A gateway status is the server being unreachable, not one
			// bad record in it. Narrowing the window then means dividing
			// by five over and over against something that will refuse
			// every size equally, which burns the caller's whole deadline
			// before it can try anywhere else. Only a plain 500 is worth
			// isolating.
			if se.Code != 500 {
				return nil, err
			}
			// The server is alive and refusing this particular window,
			// every time, in well under a second. One record in it
			// breaks Plex's own serialiser: measured, offset
			// 96000 size 1000 failed on every attempt while size 200 at
			// the same offset, and size 1000 at 95000 and 97000, all
			// succeeded.
			//
			// Retrying cannot help and giving up is worse than it
			// sounds: this is what silently dropped the whole 99,426
			// film section of the primary server out of the first
			// merged catalogue. So narrow the window until the bad
			// record is alone, step over it, and carry on.
			if size > 1 {
				size /= 5
				if size < 1 {
					size = 1
				}
				continue
			}
			log.Printf("plex: %s: record at %d is unlistable (%s), skipping it", path, start, se.Status)
			start++
			size = pageSize
			skipped++
			if skipped > maxSkips {
				return nil, fmt.Errorf("plex GET %s: gave up after skipping %d unlistable records", path, skipped)
			}
			continue
		}

		batch := out.MediaContainer.Metadata
		all = append(all, batch...)
		if len(batch) < size {
			return all, nil
		}
		start += len(batch)
	}
	return all, nil
}

// PartURL builds the token-authenticated URL for a media Part.
func (c *Client) PartURL(p Part) string {
	return fmt.Sprintf("%s%s?X-Plex-Token=%s", c.baseURL, p.Key, url.QueryEscape(c.token))
}

// OpenPart issues a GET for a media Part, optionally with a Range
// header, returning the live response for the caller to stream and close.
func (c *Client) OpenPart(ctx context.Context, p Part, rangeHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.PartURL(p), nil)
	if err != nil {
		return nil, err
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.redact(err)
	}
	return resp, nil
}

// StatusError is an HTTP status the server answered with. It is a
// distinct type because the caller has to tell two failures apart: a
// 5xx means the server is alive and refusing this particular request,
// which retrying will not fix, while a transport error means the
// provider has dropped out and waiting is exactly right.
type StatusError struct {
	Code   int
	Status string
	Path   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("plex GET %s: unexpected status %s", e.Path, e.Status)
}
