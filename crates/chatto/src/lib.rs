#![deny(unsafe_code)]

use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use reqwest::{Client as HttpClient, Method, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::json;
use std::collections::HashMap;
use std::time::Duration;
use thiserror::Error;

const ERR_TOKEN_NOT_MEMBER: &str = "not a member of this room";
const ERR_TOKEN_PERMISSION_DENIED: &str = "permission denied";

#[derive(Debug, Clone)]
pub struct Client {
    base_url: String,
    token: String,
    http_client: HttpClient,
}

impl Client {
    pub fn new(base_url: &str, token: &str) -> Self {
        Self {
            base_url: base_url.trim_end_matches('/').to_owned(),
            token: token.to_owned(),
            http_client: HttpClient::builder()
                .timeout(Duration::from_secs(30))
                .build()
                .expect("failed to build reqwest client"),
        }
    }

    pub fn base_url(&self) -> &str {
        &self.base_url
    }

    pub async fn create_message(&self, room_id: &str, body: &str) -> Result<(), Error> {
        self.do_rpc::<_, serde_json::Value>(
            "chatto.api.v1.MessageService",
            "CreateMessage",
            Some(json!({"roomId": room_id, "body": body})),
        )
        .await
        .map(|_| ())
    }

    pub async fn get_room_events(
        &self,
        room_id: &str,
        after_cursor: &str,
        limit: i32,
    ) -> Result<GetRoomEventsResponse, Error> {
        let req = if after_cursor.is_empty() {
            json!({"roomId": room_id, "limit": limit})
        } else {
            json!({"roomId": room_id, "limit": limit, "cursor": {"after": after_cursor}})
        };

        self.do_rpc("chatto.api.v1.RoomService", "GetRoomEvents", Some(req))
            .await
    }

    pub async fn get_viewer(&self) -> Result<String, Error> {
        let profile = self.get_profile().await?;
        Ok(profile.id)
    }

    pub async fn get_profile(&self) -> Result<UserProfile, Error> {
        #[derive(Debug, Deserialize)]
        struct Resp {
            user: User,
        }
        #[derive(Debug, Deserialize)]
        struct User {
            profile: UserProfile,
        }
        let resp: Resp = self
            .do_rpc("chatto.api.v1.ViewerService", "GetViewer", Some(json!({})))
            .await?;
        Ok(resp.user.profile)
    }

    pub async fn add_member(&self, room_id: &str, user_id: &str) -> Result<(), Error> {
        self.do_rpc::<_, serde_json::Value>(
            "chatto.api.v1.RoomService",
            "AddMember",
            Some(json!({"roomId": room_id, "userId": user_id})),
        )
        .await
        .map(|_| ())
    }

    pub async fn join_room(&self, room_id: &str) -> Result<(), Error> {
        self.do_rpc::<_, serde_json::Value>(
            "chatto.api.v1.RoomService",
            "JoinRoom",
            Some(json!({"roomId": room_id})),
        )
        .await
        .map(|_| ())
    }

    pub async fn join_call(&self, room_id: &str) -> Result<bool, Error> {
        #[derive(Debug, Deserialize)]
        struct Resp {
            joined: bool,
        }
        let resp: Resp = self
            .do_rpc(
                "chatto.api.v1.VoiceCallService",
                "JoinCall",
                Some(json!({"roomId": room_id})),
            )
            .await?;
        Ok(resp.joined)
    }

    pub async fn get_call_token(&self, room_id: &str) -> Result<CallToken, Error> {
        self.do_rpc(
            "chatto.api.v1.VoiceCallService",
            "GetCallToken",
            Some(json!({"roomId": room_id})),
        )
        .await
    }

    pub async fn leave_call(&self, room_id: &str) -> Result<bool, Error> {
        #[derive(Debug, Deserialize)]
        struct Resp {
            left: bool,
        }
        let resp: Resp = self
            .do_rpc(
                "chatto.api.v1.VoiceCallService",
                "LeaveCall",
                Some(json!({"roomId": room_id})),
            )
            .await?;
        Ok(resp.left)
    }

    pub async fn update_presence(&self, status: &str, user_selected: bool) -> Result<(), Error> {
        self.do_rpc::<_, serde_json::Value>(
            "chatto.api.v1.MyAccountService",
            "UpdatePresence",
            Some(json!({"status": status, "user_selected": user_selected})),
        )
        .await
        .map(|_| ())
    }

    pub async fn update_custom_status(&self, emoji: &str, text: &str) -> Result<(), Error> {
        self.do_rpc::<_, serde_json::Value>(
            "chatto.api.v1.MyAccountService",
            "UpdateCustomStatus",
            Some(json!({"emoji": emoji, "text": text})),
        )
        .await
        .map(|_| ())
    }

    pub async fn set_avatar(&self, image_data: &[u8]) -> Result<(), Error> {
        let url = format!(
            "{}/api/connect/chatto.api.v1.MyAccountService/UploadAvatar",
            self.base_url
        );

        let mut inner = Vec::with_capacity(image_data.len() + 10);
        inner.push(0x0A);
        encode_varint(&mut inner, image_data.len() as u64);
        inner.extend_from_slice(image_data);

        let mut outer = Vec::with_capacity(inner.len() + 10);
        outer.push(0x22);
        encode_varint(&mut outer, inner.len() as u64);
        outer.extend_from_slice(&inner);

        let mut request = self
            .http_client
            .request(Method::POST, &url)
            .header(CONTENT_TYPE, "application/proto")
            .header("connect-protocol-version", "1")
            .body(outer);

        if !self.token.is_empty() {
            request = request.header(AUTHORIZATION, format!("Bearer {}", self.token));
        }

        let response = request.send().await.map_err(Error::Http)?;
        let status = response.status();

        if !status.is_success() {
            let body = response.text().await.map_err(Error::Http)?;
            return Err(Error::Rpc(RpcError {
                status_code: status,
                url,
                body: truncate(&body, 500),
            }));
        }

        Ok(())
    }

    async fn do_rpc<Req, Resp>(
        &self,
        service: &str,
        method: &str,
        req: Option<Req>,
    ) -> Result<Resp, Error>
    where
        Req: Serialize,
        Resp: for<'de> Deserialize<'de>,
    {
        let url = format!("{}/api/connect/{service}/{method}", self.base_url);

        let mut request = self
            .http_client
            .request(Method::POST, &url)
            .header(CONTENT_TYPE, "application/json");

        if !self.token.is_empty() {
            request = request.header(AUTHORIZATION, format!("Bearer {}", self.token));
        }

        if let Some(req_body) = req {
            request = request.json(&req_body);
        }

        let response = request.send().await.map_err(Error::Http)?;
        let status = response.status();
        let body = response.text().await.map_err(Error::Http)?;

        if !status.is_success() {
            return Err(Error::Rpc(RpcError {
                status_code: status,
                url,
                body: truncate(&body, 500),
            }));
        }

        let parse_body = if body.trim().is_empty() {
            "null"
        } else {
            &body
        };

        serde_json::from_str(parse_body).map_err(|source| Error::Unmarshal {
            status_code: status,
            url,
            source,
            body: truncate(&body, 1000),
        })
    }
}

#[derive(Debug, Error)]
pub enum Error {
    #[error("http request: {0}")]
    Http(reqwest::Error),
    #[error(transparent)]
    Rpc(RpcError),
    #[error("unmarshal response (status {status_code}) for {url}: {source}\nbody: {body}")]
    Unmarshal {
        status_code: StatusCode,
        url: String,
        source: serde_json::Error,
        body: String,
    },
}

#[derive(Debug, Error)]
#[error("RPC error (status {status_code}) for {url}: {body}")]
pub struct RpcError {
    pub status_code: StatusCode,
    pub url: String,
    pub body: String,
}

pub fn is_not_member_error(err: &Error) -> bool {
    match err {
        Error::Rpc(rpc) => rpc.body.to_ascii_lowercase().contains(ERR_TOKEN_NOT_MEMBER),
        _ => false,
    }
}

pub fn is_permission_denied_error(err: &Error) -> bool {
    match err {
        Error::Rpc(rpc) => rpc
            .body
            .to_ascii_lowercase()
            .contains(ERR_TOKEN_PERMISSION_DENIED),
        _ => false,
    }
}

pub fn encode_varint(buf: &mut Vec<u8>, mut value: u64) {
    loop {
        if value < 0x80 {
            buf.push(value as u8);
            break;
        }
        buf.push((value as u8 & 0x7F) | 0x80);
        value >>= 7;
    }
}

pub fn truncate(s: &str, max_len: usize) -> String {
    if s.len() <= max_len {
        return s.to_owned();
    }
    let mut idx = max_len;
    while idx > 0 && !s.is_char_boundary(idx) {
        idx -= 1;
    }
    s[..idx].to_owned()
}

#[derive(Debug, Deserialize, Serialize, Clone, PartialEq, Eq)]
pub struct GetRoomEventsResponse {
    pub page: Option<RoomTimelinePage>,
}

#[derive(Debug, Deserialize, Serialize, Clone, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct UserProfile {
    pub id: String,
    #[serde(default)]
    pub display_name: Option<String>,
    #[serde(default)]
    pub avatar_url: Option<String>,
}

#[derive(Debug, Deserialize, Serialize, Clone, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct RoomTimelinePage {
    #[serde(default)]
    pub events: Vec<RoomTimelineEvent>,
    #[serde(default)]
    pub start_cursor: String,
    #[serde(default)]
    pub end_cursor: String,
    #[serde(default)]
    pub has_older: bool,
    #[serde(default)]
    pub has_newer: bool,
    #[serde(default)]
    pub includes: Option<RoomTimelineIncludes>,
}

#[derive(Debug, Deserialize, Serialize, Clone, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct RoomTimelineIncludes {
    #[serde(default)]
    pub users: HashMap<String, UserProfile>,
}

#[derive(Debug, Deserialize, Serialize, Clone, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct RoomTimelineEvent {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub created_at: String,
    #[serde(default)]
    pub actor_id: String,
    pub message_posted: Option<RoomMessagePosted>,
    pub room_created: Option<RoomEventMeta>,
    pub user_joined_room: Option<RoomEventMeta>,
}

#[derive(Debug, Deserialize, Serialize, Clone, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct RoomEventMeta {
    pub room_id: String,
}

#[derive(Debug, Deserialize, Serialize, Clone, PartialEq, Eq)]
pub struct RoomMessagePosted {
    pub message: Message,
}

#[derive(Debug, Deserialize, Serialize, Clone, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct Message {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub room_id: String,
    #[serde(default)]
    pub actor_id: String,
    pub body: Option<String>,
}

#[derive(Debug, Deserialize, Serialize, Clone, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct CallToken {
    pub token: String,
    pub e2ee_key: String,
    pub call_id: String,
}

#[cfg(test)]
mod tests {
    use super::*;
    use mockito::{Matcher, Server};

    #[test]
    fn test_truncate() {
        assert_eq!(truncate("hello", 10), "hello");
        assert_eq!(truncate("hello", 3), "hel");
        assert_eq!(truncate("hello", 0), "");
        assert_eq!(truncate("héllo", 4), "hél");
        assert_eq!(truncate("世界", 4), "世");
    }

    #[test]
    fn test_new_client_trims_trailing_slash() {
        let c = Client::new("https://chat.example.com/", "tok_abc");
        assert_eq!(c.base_url(), "https://chat.example.com");
    }

    #[test]
    fn test_error_helpers() {
        let err = Error::Rpc(RpcError {
            status_code: StatusCode::FORBIDDEN,
            url: "/x".to_owned(),
            body: "Not a member of this room".to_owned(),
        });
        assert!(is_not_member_error(&err));
        assert!(!is_permission_denied_error(&err));

        let err = Error::Rpc(RpcError {
            status_code: StatusCode::FORBIDDEN,
            url: "/x".to_owned(),
            body: "Permission Denied".to_owned(),
        });
        assert!(is_permission_denied_error(&err));
        assert!(!is_not_member_error(&err));
    }

    #[tokio::test]
    async fn test_get_viewer_auth_header() {
        let mut server = Server::new_async().await;
        let mock = server
            .mock("POST", "/api/connect/chatto.api.v1.ViewerService/GetViewer")
            .match_header("content-type", "application/json")
            .match_header("authorization", "Bearer tok_abc")
            .with_status(200)
            .with_body(r#"{"user":{"profile":{"id":"usr_123"}}}"#)
            .create_async()
            .await;

        let c = Client::new(&server.url(), "tok_abc");
        let id = c.get_viewer().await.unwrap();
        assert_eq!(id, "usr_123");
        mock.assert_async().await;
    }

    #[tokio::test]
    async fn test_no_auth_when_token_empty() {
        let mut server = Server::new_async().await;
        let mock = server
            .mock("POST", "/api/connect/chatto.api.v1.ViewerService/GetViewer")
            .match_header("authorization", Matcher::Missing)
            .with_status(200)
            .with_body(r#"{"user":{"profile":{"id":"usr_1"}}}"#)
            .create_async()
            .await;

        let c = Client::new(&server.url(), "");
        let _ = c.get_viewer().await.unwrap();
        mock.assert_async().await;
    }

    #[tokio::test]
    async fn test_rpc_error_status() {
        let mut server = Server::new_async().await;
        let mock = server
            .mock("POST", "/api/connect/chatto.api.v1.ViewerService/GetViewer")
            .with_status(403)
            .with_body("forbidden")
            .create_async()
            .await;

        let c = Client::new(&server.url(), "tok");
        let err = c.get_viewer().await.unwrap_err();
        match err {
            Error::Rpc(rpc) => {
                assert_eq!(rpc.status_code, StatusCode::FORBIDDEN);
                assert!(rpc.body.contains("forbidden"));
            }
            other => panic!("expected rpc error, got: {other}"),
        }

        mock.assert_async().await;
    }

    #[tokio::test]
    async fn test_unmarshal_error() {
        let mut server = Server::new_async().await;
        let mock = server
            .mock("POST", "/api/connect/chatto.api.v1.ViewerService/GetViewer")
            .with_status(200)
            .with_body("not json")
            .create_async()
            .await;

        let c = Client::new(&server.url(), "tok");
        let err = c.get_viewer().await.unwrap_err();
        match err {
            Error::Unmarshal { .. } => {}
            other => panic!("expected unmarshal error, got: {other}"),
        }
        mock.assert_async().await;
    }

    #[tokio::test]
    async fn test_get_room_events_with_cursor() {
        let mut server = Server::new_async().await;
        let mock = server
            .mock(
                "POST",
                "/api/connect/chatto.api.v1.RoomService/GetRoomEvents",
            )
            .match_body(Matcher::JsonString(
                r#"{"cursor":{"after":"c1"},"limit":10,"roomId":"room1"}"#.to_owned(),
            ))
            .with_status(200)
            .with_body(r#"{"page":{"events":[{"id":"evt1"}],"endCursor":"c2"}}"#)
            .create_async()
            .await;

        let c = Client::new(&server.url(), "tok");
        let resp = c.get_room_events("room1", "c1", 10).await.unwrap();
        assert_eq!(resp.page.unwrap().end_cursor, "c2");
        mock.assert_async().await;
    }

    #[tokio::test]
    async fn test_get_room_events_no_cursor() {
        let mut server = Server::new_async().await;
        let mock = server
            .mock(
                "POST",
                "/api/connect/chatto.api.v1.RoomService/GetRoomEvents",
            )
            .match_body(Matcher::JsonString(
                r#"{"limit":10,"roomId":"room1"}"#.to_owned(),
            ))
            .with_status(200)
            .with_body(r#"{"page":{"events":[],"endCursor":"c1"}}"#)
            .create_async()
            .await;

        let c = Client::new(&server.url(), "tok");
        let resp = c.get_room_events("room1", "", 10).await.unwrap();
        assert_eq!(resp.page.unwrap().end_cursor, "c1");
        mock.assert_async().await;
    }

    #[tokio::test]
    async fn test_voice_methods() {
        let mut server = Server::new_async().await;

        let join = server
            .mock(
                "POST",
                "/api/connect/chatto.api.v1.VoiceCallService/JoinCall",
            )
            .with_status(200)
            .with_body(r#"{"joined":true}"#)
            .create_async()
            .await;

        let token = server
            .mock(
                "POST",
                "/api/connect/chatto.api.v1.VoiceCallService/GetCallToken",
            )
            .with_status(200)
            .with_body(r#"{"token":"jwt_token","e2eeKey":"key","callId":"call_1"}"#)
            .create_async()
            .await;

        let leave = server
            .mock(
                "POST",
                "/api/connect/chatto.api.v1.VoiceCallService/LeaveCall",
            )
            .with_status(200)
            .with_body(r#"{"left":true}"#)
            .create_async()
            .await;

        let c = Client::new(&server.url(), "tok");
        assert!(c.join_call("room1").await.unwrap());
        let tk = c.get_call_token("room1").await.unwrap();
        assert_eq!(tk.token, "jwt_token");
        assert_eq!(tk.e2ee_key, "key");
        assert_eq!(tk.call_id, "call_1");
        assert!(c.leave_call("room1").await.unwrap());

        join.assert_async().await;
        token.assert_async().await;
        leave.assert_async().await;
    }

    #[tokio::test]
    async fn test_message_and_presence_methods() {
        let mut server = Server::new_async().await;

        let create_msg = server
            .mock(
                "POST",
                "/api/connect/chatto.api.v1.MessageService/CreateMessage",
            )
            .match_body(Matcher::JsonString(
                r#"{"body":"hello","roomId":"room1"}"#.to_owned(),
            ))
            .with_status(200)
            .with_body("null")
            .create_async()
            .await;

        let presence = server
            .mock(
                "POST",
                "/api/connect/chatto.api.v1.MyAccountService/UpdatePresence",
            )
            .match_body(Matcher::JsonString(
                r#"{"status":"ONLINE","user_selected":true}"#.to_owned(),
            ))
            .with_status(200)
            .with_body("null")
            .create_async()
            .await;

        let custom = server
            .mock(
                "POST",
                "/api/connect/chatto.api.v1.MyAccountService/UpdateCustomStatus",
            )
            .with_status(200)
            .with_body("null")
            .create_async()
            .await;

        let c = Client::new(&server.url(), "tok");
        c.create_message("room1", "hello").await.unwrap();
        c.update_presence("ONLINE", true).await.unwrap();
        c.update_custom_status("x", "listening").await.unwrap();

        create_msg.assert_async().await;
        presence.assert_async().await;
        custom.assert_async().await;
    }
}
