package chatto

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		s      string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"héllo", 4, "hél"},
		{"世界", 4, "世"},
		{"", 5, ""},
	}

	for _, tc := range tests {
		got := truncate(tc.s, tc.maxLen)
		if got != tc.want {
			t.Fatalf("truncate(%q, %d) = %q, want %q", tc.s, tc.maxLen, got, tc.want)
		}
	}
}

func TestNewClient(t *testing.T) {
	c := NewClient("https://chat.example.com/", "tok_abc")
	if c.baseURL != "https://chat.example.com" {
		t.Fatalf("expected trimmed base URL, got %q", c.baseURL)
	}
	if c.token != "tok_abc" {
		t.Fatalf("expected token tok_abc, got %q", c.token)
	}
}

func TestNewClientNoTrailingSlash(t *testing.T) {
	c := NewClient("https://chat.example.com", "tok")
	if c.baseURL != "https://chat.example.com" {
		t.Fatalf("expected no trailing slash, got %q", c.baseURL)
	}
}

func TestRPCError(t *testing.T) {
	err := &RPCError{StatusCode: 403, URL: "/api/connect/test/Method", Body: "forbidden"}
	want := "RPC error (status 403) for /api/connect/test/Method: forbidden"
	if err.Error() != want {
		t.Fatalf("RPCError.Error() = %q, want %q", err.Error(), want)
	}
}

func TestIsNotMemberError(t *testing.T) {
	if IsNotMemberError(nil) {
		t.Fatal("expected false for nil")
	}
	if IsNotMemberError(&RPCError{Body: "permission denied"}) {
		t.Fatal("expected false for non-member error")
	}
	if !IsNotMemberError(&RPCError{Body: "not a member of this room"}) {
		t.Fatal("expected true for member error")
	}
	if !IsNotMemberError(&RPCError{Body: "Not a member of this room"}) {
		t.Fatal("expected case-insensitive match")
	}
}

func TestIsPermissionDeniedError(t *testing.T) {
	if IsPermissionDeniedError(nil) {
		t.Fatal("expected false for nil")
	}
	if IsPermissionDeniedError(&RPCError{Body: "not a member"}) {
		t.Fatal("expected false for permission error")
	}
	if !IsPermissionDeniedError(&RPCError{Body: "permission denied"}) {
		t.Fatal("expected true for permission error")
	}
	if !IsPermissionDeniedError(&RPCError{Body: "Permission Denied"}) {
		t.Fatal("expected case-insensitive match")
	}
}

func TestDoRPCSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Authorization") != "Bearer tok_abc" {
			t.Fatalf("expected Bearer tok_abc, got %s", r.Header.Get("Authorization"))
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok_abc")
	var resp struct {
		Result string `json:"result"`
	}
	if err := c.doRPC(context.Background(), "TestService", "TestMethod", map[string]string{"key": "val"}, &resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Result != "ok" {
		t.Fatalf("expected result ok, got %q", resp.Result)
	}
}

func TestDoRPCNoAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("expected no auth header, got %s", r.Header.Get("Authorization"))
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	if err := c.doRPC(context.Background(), "TestService", "TestMethod", nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDoRPCErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`forbidden`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	err := c.doRPC(context.Background(), "TestService", "TestMethod", nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *RPCError
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("expected 'forbidden' in error, got %v", err)
	}
	if _, ok := err.(*RPCError); ok {
		rpcErr = err.(*RPCError)
		if rpcErr.StatusCode != 403 {
			t.Fatalf("expected status 403, got %d", rpcErr.StatusCode)
		}
	}
}

func TestDoRPCUnmarshalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	var resp struct{}
	err := c.doRPC(context.Background(), "TestService", "TestMethod", nil, &resp)
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestGetRoomEventsWithCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["cursor"] == nil {
			t.Fatal("expected cursor in request")
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"page":{"events":[{"id":"evt1"}],"endCursor":"c2"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	resp, err := c.GetRoomEvents(context.Background(), "room1", "c1", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Page.EndCursor != "c2" {
		t.Fatalf("expected cursor c2, got %q", resp.Page.EndCursor)
	}
	if len(resp.Page.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(resp.Page.Events))
	}
}

func TestGetRoomEventsNoCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["cursor"] != nil {
			t.Fatal("expected no cursor in request")
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"page":{"events":[],"endCursor":"c1"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	resp, err := c.GetRoomEvents(context.Background(), "room1", "", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Page.EndCursor != "c1" {
		t.Fatalf("expected cursor c1, got %q", resp.Page.EndCursor)
	}
}

func TestGetViewer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"user":{"profile":{"id":"usr_123"}}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	id, err := c.GetViewer(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "usr_123" {
		t.Fatalf("expected usr_123, got %q", id)
	}
}

func TestJoinCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"joined":true}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	joined, err := c.JoinCall(context.Background(), "room1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !joined {
		t.Fatal("expected joined=true")
	}
}

func TestLeaveCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"left":true}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	left, err := c.LeaveCall(context.Background(), "room1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !left {
		t.Fatal("expected left=true")
	}
}

func TestGetCallToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"token":"jwt_token","e2eeKey":"key","callId":"call_1"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	token, err := c.GetCallToken(context.Background(), "room1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token.Token != "jwt_token" {
		t.Fatalf("expected jwt_token, got %q", token.Token)
	}
	if token.E2EEKey != "key" {
		t.Fatalf("expected key, got %q", token.E2EEKey)
	}
	if token.CallID != "call_1" {
		t.Fatalf("expected call_1, got %q", token.CallID)
	}
}

func TestAddMember(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["roomId"] != "room1" {
			t.Fatalf("expected roomId room1, got %v", req["roomId"])
		}
		if req["userId"] != "usr_1" {
			t.Fatalf("expected userId usr_1, got %v", req["userId"])
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if err := c.AddMember(context.Background(), "room1", "usr_1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["roomId"] != "room1" {
			t.Fatalf("expected roomId room1, got %v", req["roomId"])
		}
		if req["body"] != "hello" {
			t.Fatalf("expected body hello, got %v", req["body"])
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if err := c.CreateMessage(context.Background(), "room1", "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpdatePresence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["status"] != "ONLINE" {
			t.Fatalf("expected status ONLINE, got %v", req["status"])
		}
		if req["user_selected"] != true {
			t.Fatalf("expected user_selected true, got %v", req["user_selected"])
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if err := c.UpdatePresence(context.Background(), "ONLINE", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpdateCustomStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["emoji"] != "🎧" {
			t.Fatalf("expected emoji 🎧, got %v", req["emoji"])
		}
		if req["text"] != "listening" {
			t.Fatalf("expected text listening, got %v", req["text"])
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if err := c.UpdateCustomStatus(context.Background(), "🎧", "listening"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
