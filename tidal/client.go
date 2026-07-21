package tidal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/binozo/go-tiddl"
)

const tidalAPIBase = "https://api.tidal.com/v1"

type Client struct {
	tiddl  *tiddl.Client
	http   *http.Client
	token  string
}

func NewClient(ctx context.Context, tokenPath string) (*Client, error) {
	// Try saved token first
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

	// GetSession may return 403 on some Tidal configurations;
	// hardcode a default country code and try to fetch the real one.
	tc.CountryCode = "US"
	if session, err := tc.GetSession(ctx); err == nil {
		tc.CountryCode = session.CountryCode
	} else {
		slog.Warn("could not get tidal session, using default country code", "error", err, "country", tc.CountryCode)
	}

	tok, _ := tc.Token()
	return &Client{
		tiddl: tc,
		http:  &http.Client{Timeout: 20 * time.Second},
		token: tok.AccessToken,
	}, nil
}

type tidalTrack struct {
	ID         uint64 `json:"id"`
	Title      string `json:"title"`
	ArtistName string `json:"artistName"`
	Duration   int    `json:"duration"`
	Artists    []struct {
		Name string `json:"name"`
	} `json:"artists"`
}

type searchResponse struct {
	Tracks *struct {
		Items []tidalTrack `json:"items"`
		Total int          `json:"total"`
	} `json:"tracks"`
}

func (c *Client) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	u := fmt.Sprintf("%s/search/tracks?query=%s&limit=%d&offset=0",
		tidalAPIBase, url.QueryEscape(query), limit)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create search request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request: %w", err)
	}
	defer resp.Body.Close()

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

	if sr.Tracks == nil {
		return nil, nil
	}

	results := make([]SearchResult, 0, len(sr.Tracks.Items))
	for _, t := range sr.Tracks.Items {
		artist := t.ArtistName
		if artist == "" && len(t.Artists) > 0 {
			artist = t.Artists[0].Name
		}
		results = append(results, SearchResult{
			ID:       t.ID,
			Title:    t.Title,
			Artist:   artist,
			Duration: t.Duration,
		})
	}
	return results, nil
}

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

func (c *Client) StreamTrack(ctx context.Context, id uint64) (*TrackStream, error) {
	track, err := c.tiddl.GetTrack(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get track: %w", err)
	}

	quality := track.BestQuality()
	stream, err := c.tiddl.GetTrackStream(ctx, uint64(track.ID), quality, false)
	if err != nil {
		return nil, fmt.Errorf("get track stream: %w", err)
	}

	reader, err := c.tiddl.DownloadTrackStream(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("download track stream: %w", err)
	}

	return &TrackStream{
		Reader:   reader,
		Duration: track.Duration,
	}, nil
}

func (c *Client) Close() error {
	return c.tiddl.Close()
}
