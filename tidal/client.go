// Package tidal provides a client for the Tidal API, including search,
// track metadata, audio streaming, and URL parsing.
package tidal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/binozo/go-tiddl"
	"golang.org/x/oauth2"
)

const tidalAPIBase = "https://api.tidal.com/v1"

// Client wraps a go-tiddl client with an authenticated HTTP client for
// direct Tidal REST API calls (search, album/playlist/artist endpoints).
type Client struct {
	tiddl   *tiddl.Client
	http    *http.Client
	quality tiddl.AudioQuality
}

// NewClient creates a new Tidal client, restoring a saved token if available
// or performing device authorization flow if no token exists.
// The quality parameter sets the desired stream quality; use an empty string
// to always pick the best available quality per track.
func NewClient(ctx context.Context, tokenPath string, quality string) (*Client, error) {
	token := loadSavedToken(tokenPath)

	if token == nil {
		var err error
		token, err = deviceAuth(ctx, tokenPath)
		if err != nil {
			return nil, fmt.Errorf("tidal login: %w", err)
		}
	}

	tc, err := tiddl.NewClient(
		tiddl.WithContext(ctx),
		tiddl.WithLogger(slog.Default()),
		tiddl.WithAuthToken(token),
	)
	if err != nil {
		return nil, fmt.Errorf("create tidal client: %w", err)
	}

	tc.SetTokenChanged(func(tok *oauth2.Token) {
		saveToken(tokenPath, tok)
	})

	tc.CountryCode = "US"
	if session, err := tc.GetSession(ctx); err == nil {
		tc.CountryCode = session.CountryCode
	} else {
		slog.Warn("could not get tidal session, using default country code", "error", err, "country", tc.CountryCode)
	}

	tok, _ := tc.Token()
	src := oauth2.StaticTokenSource(tok)
	httpClient := oauth2.NewClient(ctx, src)
	httpClient.Timeout = 20 * time.Second

	c := &Client{
		tiddl:   tc,
		http:    httpClient,
		quality: tiddl.AudioQuality(quality),
	}

	if quality == "" {
		c.quality = ""
	}

	return c, nil
}

// tidalTrack is the raw JSON shape returned by Tidal REST API endpoints
// for a track item.
type tidalTrack struct {
	ID         uint64 `json:"id"`
	Title      string `json:"title"`
	ArtistName string `json:"artistName"`
	Duration   int    `json:"duration"`
	Artists    []struct {
		Name string `json:"name"`
	} `json:"artists"`
}

// artistName extracts the best available artist name from a tidalTrack,
// prefering ArtistName but falling back to the first entry in Artists.
func artistName(t tidalTrack) string {
	if t.ArtistName != "" {
		return t.ArtistName
	}
	if len(t.Artists) > 0 {
		return t.Artists[0].Name
	}
	return ""
}

// searchResponse wraps the JSON shape returned by the Tidal track search endpoint.
type searchResponse struct {
	Items []tidalTrack `json:"items"`
}

// Search queries Tidal's track search and returns up to limit results.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	u := fmt.Sprintf("%s/search/tracks?query=%s&limit=%d&offset=0&countryCode=%s",
		tidalAPIBase, url.QueryEscape(query), limit, c.tiddl.CountryCode)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create search request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read search response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("search API error (status %d): %s", resp.StatusCode, string(body))
	}

	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("parse search response: %w", err)
	}

	if len(sr.Items) == 0 {
		slog.Debug("search: no items in response", "query", query, "body", string(body))
		return nil, nil
	}

	results := make([]SearchResult, 0, len(sr.Items))
	for _, t := range sr.Items {
		results = append(results, SearchResult{
			ID:       t.ID,
			Title:    t.Title,
			Artist:   artistName(t),
			Duration: t.Duration,
		})
	}
	return results, nil
}

// GetTrack fetches a single track's metadata by its Tidal ID.
func (c *Client) GetTrack(ctx context.Context, id uint64) (*Track, error) {
	t, err := c.tiddl.GetTrack(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get track: %w", err)
	}

	artist := ""
	if len(t.Artists) > 0 {
		artist = t.Artists[0].Name
	}

	return &Track{
		ID:       uint64(t.ID),
		Title:    t.Title,
		Artist:   artist,
		Duration: t.Duration,
	}, nil
}

// StreamTrack opens an audio stream for the given track ID, returning
// the stream reader and the track's duration.
func (c *Client) StreamTrack(ctx context.Context, id uint64) (*TrackStream, error) {
	track, err := c.tiddl.GetTrack(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get track: %w", err)
	}

	quality := c.quality
	if quality == "" {
		quality = track.BestQuality()
	}
	slog.Info("streaming track", "title", track.Title, "quality", quality, "duration", track.Duration)
	stream, err := c.tiddl.GetTrackStream(ctx, uint64(track.ID), quality, false)
	if err != nil {
		return nil, fmt.Errorf("get track stream: %w", err)
	}

	slog.Info("got track stream manifest", "quality", quality, "manifest_type", stream.ManifestMimeType)

	codec := stream.Manifest.GetCodecs()
	var bitDepth, sampleRate int
	if stream.Info != nil {
		bitDepth = stream.Info.BitDepth
		sampleRate = stream.Info.SampleRate
	}

	reader, err := c.tiddl.DownloadTrackStream(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("download track stream: %w", err)
	}

	return &TrackStream{
		Reader:     reader,
		Duration:   track.Duration,
		Quality:    string(quality),
		Codec:      codec,
		BitDepth:   bitDepth,
		SampleRate: sampleRate,
	}, nil
}

// GetAlbumTracks fetches all tracks in an album by album ID.
func (c *Client) GetAlbumTracks(ctx context.Context, albumID uint64) ([]SearchResult, error) {
	u := fmt.Sprintf("%s/albums/%d/items?limit=100&offset=0&countryCode=%s",
		tidalAPIBase, albumID, c.tiddl.CountryCode)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create album items request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("album items request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read album items response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("album items API error (status %d): %s", resp.StatusCode, string(body))
	}

	var data struct {
		Items []struct {
			Item tidalTrack `json:"item"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parse album items: %w", err)
	}

	results := make([]SearchResult, 0, len(data.Items))
	for _, it := range data.Items {
		t := it.Item
		results = append(results, SearchResult{
			ID:       t.ID,
			Title:    t.Title,
			Artist:   artistName(t),
			Duration: t.Duration,
		})
	}
	return results, nil
}

// GetPlaylistTracks fetches all tracks in a playlist by playlist UUID.
func (c *Client) GetPlaylistTracks(ctx context.Context, playlistID string) ([]SearchResult, error) {
	u := fmt.Sprintf("%s/playlists/%s/items?limit=100&offset=0&countryCode=%s",
		tidalAPIBase, url.PathEscape(playlistID), c.tiddl.CountryCode)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create playlist items request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("playlist items request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read playlist items response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("playlist items API error (status %d): %s", resp.StatusCode, string(body))
	}

	var data struct {
		Items []struct {
			Item tidalTrack `json:"item"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parse playlist items: %w", err)
	}

	results := make([]SearchResult, 0, len(data.Items))
	for _, it := range data.Items {
		t := it.Item
		results = append(results, SearchResult{
			ID:       t.ID,
			Title:    t.Title,
			Artist:   artistName(t),
			Duration: t.Duration,
		})
	}
	return results, nil
}

// GetArtistTopTracks fetches an artist's top tracks by artist ID.
func (c *Client) GetArtistTopTracks(ctx context.Context, artistID uint64) ([]SearchResult, error) {
	u := fmt.Sprintf("%s/artists/%d/toptracks?limit=50&offset=0&countryCode=%s",
		tidalAPIBase, artistID, c.tiddl.CountryCode)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create artist top tracks request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artist top tracks request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read artist top tracks response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("artist top tracks API error (status %d): %s", resp.StatusCode, string(body))
	}

	var data struct {
		Items []tidalTrack `json:"items"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parse artist top tracks: %w", err)
	}

	results := make([]SearchResult, 0, len(data.Items))
	for _, t := range data.Items {
		results = append(results, SearchResult{
			ID:       t.ID,
			Title:    t.Title,
			Artist:   artistName(t),
			Duration: t.Duration,
		})
	}
	return results, nil
}

// ParseTidalURL parses a Tidal share URL and returns the content type
// ("track", "album", "playlist", "artist") and the content ID.
//
// Supported formats:
//
//	https://tidal.com/track/123
//	https://tidal.com/browse/track/123
//	https://tidal.com/track/123/u
//	track/123  (scheme-less)
func ParseTidalURL(rawURL string) (contentType string, id string, err error) {
	if rawURL == "" {
		return "", "", fmt.Errorf("empty URL")
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("parse URL: %w", err)
	}
	if !strings.HasSuffix(u.Hostname(), "tidal.com") {
		return "", "", fmt.Errorf("not a tidal.com URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "browse" {
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return "", "", fmt.Errorf("unrecognized Tidal URL path: %s", u.Path)
	}
	contentType = parts[0]
	id = parts[1]
	switch contentType {
	case "track", "album", "playlist", "artist":
		return contentType, id, nil
	default:
		return "", "", fmt.Errorf("unsupported Tidal content type: %s", contentType)
	}
}

// Close shuts down the underlying Tidal client and releases resources.
func (c *Client) Close() error {
	return c.tiddl.Close()
}
