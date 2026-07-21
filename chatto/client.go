package chatto

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	for utf8.ValidString(s) && len(s) > maxLen {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

type Client struct {
	baseURL   string
	token     string
	httpClient *http.Client
}

func NewClient(baseURL, token string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		baseURL:   baseURL,
		token:     token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) doRPC(ctx context.Context, service, method string, req, resp any) error {
	url := fmt.Sprintf("%s/api/connect/%s/%s", c.baseURL, service, method)
	slog.Debug("chatto RPC", "url", url, "method", method)

	var body io.Reader
	if req != nil {
		data, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return fmt.Errorf("RPC error (status %d) for %s: %s", httpResp.StatusCode, url, truncate(string(respBody), 500))
	}

	if resp != nil {
		if err := json.Unmarshal(respBody, resp); err != nil {
			return fmt.Errorf("unmarshal response (status %d) for %s: %w\nbody: %s", httpResp.StatusCode, url, err, truncate(string(respBody), 1000))
		}
	}

	return nil
}

func (c *Client) CreateMessage(ctx context.Context, roomID, body string) error {
	req := map[string]any{
		"room_id": roomID,
		"body":    body,
	}
	return c.doRPC(ctx, "chatto.api.v1.MessageService", "CreateMessage", req, nil)
}

func (c *Client) GetRoomEvents(ctx context.Context, roomID, afterCursor string, limit int32) (*GetRoomEventsResponse, error) {
	req := map[string]any{
		"room_id": roomID,
		"limit":   limit,
	}
	if afterCursor != "" {
		req["cursor"] = map[string]string{"after": afterCursor}
	}

	var resp GetRoomEventsResponse
	if err := c.doRPC(ctx, "chatto.api.v1.RoomService", "GetRoomEvents", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

type GetRoomEventsResponse struct {
	Page *RoomTimelinePage `json:"page"`
}

type RoomTimelinePage struct {
	Events     []RoomTimelineEvent `json:"events"`
	StartCursor string             `json:"start_cursor"`
	EndCursor   string             `json:"end_cursor"`
	HasOlder    bool               `json:"has_older"`
	HasNewer    bool               `json:"has_newer"`
}

type RoomTimelineEvent struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	ActorID   string `json:"actor_id"`
	Event     struct {
		MessagePosted *RoomMessagePosted `json:"message_posted"`
	} `json:"event"`
}

type RoomMessagePosted struct {
	Message Message `json:"message"`
}

type Message struct {
	ID      string  `json:"id"`
	RoomID  string  `json:"room_id"`
	ActorID string  `json:"actor_id"`
	Body    *string `json:"body"`
}

func (c *Client) GetViewer(ctx context.Context) (string, error) {
	var resp struct {
		User struct {
			Profile struct {
				ID string `json:"id"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.doRPC(ctx, "chatto.api.v1.ViewerService", "GetViewer", struct{}{}, &resp); err != nil {
		return "", err
	}
	return resp.User.Profile.ID, nil
}

func (c *Client) AddMember(ctx context.Context, roomID, userID string) error {
	req := map[string]any{"room_id": roomID, "user_id": userID}
	return c.doRPC(ctx, "chatto.api.v1.RoomService", "AddMember", req, nil)
}

func (c *Client) JoinCall(ctx context.Context, roomID string) (bool, error) {
	req := map[string]any{"room_id": roomID}
	var resp struct {
		Joined bool `json:"joined"`
	}
	if err := c.doRPC(ctx, "chatto.api.v1.VoiceCallService", "JoinCall", req, &resp); err != nil {
		return false, err
	}
	return resp.Joined, nil
}

func (c *Client) GetCallToken(ctx context.Context, roomID string) (*CallToken, error) {
	req := map[string]any{"room_id": roomID}
	var resp CallToken
	if err := c.doRPC(ctx, "chatto.api.v1.VoiceCallService", "GetCallToken", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

type CallToken struct {
	Token   string `json:"token"`
	E2EEKey string `json:"e2ee_key"`
	CallID  string `json:"call_id"`
}

func (c *Client) LeaveCall(ctx context.Context, roomID string) (bool, error) {
	req := map[string]any{"room_id": roomID}
	var resp struct {
		Left bool `json:"left"`
	}
	if err := c.doRPC(ctx, "chatto.api.v1.VoiceCallService", "LeaveCall", req, &resp); err != nil {
		return false, err
	}
	return resp.Left, nil
}
