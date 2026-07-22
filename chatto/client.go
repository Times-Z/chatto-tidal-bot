// Package chatto provides a client for the Chatto ConnectRPC-over-HTTP+JSON API.
// It handles authentication, room events, messaging, voice calls, and presence.
package chatto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	errTokenNotMember        = "not a member of this room"
	errTokenPermissionDenied = "permission denied"
)

// truncate cuts a string to a maximum byte length, ensuring valid UTF-8.
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

// Client communicates with the Chatto server over HTTP+JSON RPC.
// All Chatto API services (Message, Room, VoiceCall, Viewer, MyAccount)
// are exposed as methods on this struct.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// RPCError wraps non-2xx responses returned by Chatto Connect RPC endpoints.
type RPCError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("RPC error (status %d) for %s: %s", e.StatusCode, e.URL, e.Body)
}

// IsNotMemberError reports whether an error indicates the caller is not a room member.
func IsNotMemberError(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return strings.Contains(strings.ToLower(rpcErr.Body), errTokenNotMember)
}

// IsPermissionDeniedError reports whether an error indicates missing permissions.
func IsPermissionDeniedError(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return strings.Contains(strings.ToLower(rpcErr.Body), errTokenPermissionDenied)
}

// NewClient creates a new Chatto API client for the given server URL and bearer token.
func NewClient(baseURL, token string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		baseURL:    baseURL,
		token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// doRPC sends a JSON body to Chatto's ConnectRPC HTTP endpoint at
// /api/connect/{service}/{method} and unmarshals the response.
// It mimics ConnectRPC's unary protocol using plain JSON (not protobuf).
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
	defer func() { _ = httpResp.Body.Close() }()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if strings.Contains(service, "RoomService") {
		slog.Debug("raw response", "url", url, "body", truncate(string(respBody), 2000))
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return &RPCError{
			StatusCode: httpResp.StatusCode,
			URL:        url,
			Body:       truncate(string(respBody), 500),
		}
	}

	if resp != nil {
		if err := json.Unmarshal(respBody, resp); err != nil {
			return fmt.Errorf("unmarshal response (status %d) for %s: %w\nbody: %s", httpResp.StatusCode, url, err, truncate(string(respBody), 1000))
		}
	}

	return nil
}

// CreateMessage sends a chat message to the specified room.
func (c *Client) CreateMessage(ctx context.Context, roomID, body string) error {
	req := map[string]any{
		"roomId": roomID,
		"body":   body,
	}
	return c.doRPC(ctx, "chatto.api.v1.MessageService", "CreateMessage", req, nil)
}

// GetRoomEvents retrieves timeline events for a room, with optional cursor-based pagination.
func (c *Client) GetRoomEvents(ctx context.Context, roomID, afterCursor string, limit int32) (*GetRoomEventsResponse, error) {
	req := map[string]any{
		"roomId": roomID,
		"limit":  limit,
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

// GetRoomEventsResponse wraps the paginated room timeline.
type GetRoomEventsResponse struct {
	Page *RoomTimelinePage `json:"page"`
}

// RoomTimelinePage is a cursor-paginated page of room timeline events.
type RoomTimelinePage struct {
	Events      []RoomTimelineEvent `json:"events"`
	StartCursor string              `json:"startCursor"`
	EndCursor   string              `json:"endCursor"`
	HasOlder    bool                `json:"hasOlder"`
	HasNewer    bool                `json:"hasNewer"`
}

// RoomTimelineEvent is a single event in the room timeline (message, join, etc.).
type RoomTimelineEvent struct {
	ID             string             `json:"id"`
	CreatedAt      string             `json:"createdAt"`
	ActorID        string             `json:"actorId"`
	MessagePosted  *RoomMessagePosted `json:"messagePosted,omitempty"`
	RoomCreated    *RoomEventMeta     `json:"roomCreated,omitempty"`
	UserJoinedRoom *RoomEventMeta     `json:"userJoinedRoom,omitempty"`
}

// RoomEventMeta contains minimal metadata for room-level events.
type RoomEventMeta struct {
	RoomID string `json:"roomId"`
}

// RoomMessagePosted wraps a message that was posted to a room.
type RoomMessagePosted struct {
	Message Message `json:"message"`
}

// Message represents a chat message in a room.
type Message struct {
	ID      string  `json:"id"`
	RoomID  string  `json:"roomId"`
	ActorID string  `json:"actorId"`
	Body    *string `json:"body"`
}

// GetViewer returns the authenticated user's profile ID.
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

// AddMember adds a user to a room.
func (c *Client) AddMember(ctx context.Context, roomID, userID string) error {
	req := map[string]any{"roomId": roomID, "userId": userID}
	return c.doRPC(ctx, "chatto.api.v1.RoomService", "AddMember", req, nil)
}

// JoinCall makes the bot join the voice call in the specified room.
// Returns true if the bot successfully joined the call.
func (c *Client) JoinCall(ctx context.Context, roomID string) (bool, error) {
	req := map[string]any{"roomId": roomID}
	var resp struct {
		Joined bool `json:"joined"`
	}
	if err := c.doRPC(ctx, "chatto.api.v1.VoiceCallService", "JoinCall", req, &resp); err != nil {
		return false, err
	}
	return resp.Joined, nil
}

// GetCallToken retrieves a LiveKit JWT token for the bot to connect
// as a publisher in the specified room's voice call.
func (c *Client) GetCallToken(ctx context.Context, roomID string) (*CallToken, error) {
	req := map[string]any{"roomId": roomID}
	var resp CallToken
	if err := c.doRPC(ctx, "chatto.api.v1.VoiceCallService", "GetCallToken", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CallToken contains the LiveKit JWT and related call metadata.
type CallToken struct {
	Token   string `json:"token"`
	E2EEKey string `json:"e2eeKey"`
	CallID  string `json:"callId"`
}

// LeaveCall leaves the voice call in the specified room.
// Returns true if the bot successfully left the call.
func (c *Client) LeaveCall(ctx context.Context, roomID string) (bool, error) {
	req := map[string]any{"roomId": roomID}
	var resp struct {
		Left bool `json:"left"`
	}
	if err := c.doRPC(ctx, "chatto.api.v1.VoiceCallService", "LeaveCall", req, &resp); err != nil {
		return false, err
	}
	return resp.Left, nil
}

// UpdatePresence sets the bot's online presence status on the Chatto server.
func (c *Client) UpdatePresence(ctx context.Context, status string, userSelected bool) error {
	req := map[string]any{
		"status":        status,
		"user_selected": userSelected,
	}
	return c.doRPC(ctx, "chatto.api.v1.MyAccountService", "UpdatePresence", req, nil)
}

// UpdateCustomStatus sets the bot's emoji + text status on the Chatto server.
func (c *Client) UpdateCustomStatus(ctx context.Context, emoji, text string) error {
	req := map[string]any{"emoji": emoji, "text": text}
	return c.doRPC(ctx, "chatto.api.v1.MyAccountService", "UpdateCustomStatus", req, nil)
}
