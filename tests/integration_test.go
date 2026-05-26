// Package tests contains end-to-end integration tests for protoc-gen-go-graphql.
//
// Architecture under test:
//
//	real GraphQL HTTP client → GraphQL server (gqlgen, generated) → mock gRPC handler → back
//	real WebSocket (graphql-transport-ws) client → GraphQL server → mock gRPC handler → back
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/gorilla/websocket"
	pb "github.com/gopherex/protoc-gen-go-graphql/example/gen"
	"github.com/gopherex/protoc-gen-go-graphql/example/gen/gqlapi"
	"github.com/gopherex/protoc-gen-go-graphql/example/gen/gqlapi/exec"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ─── Mock gRPC handler ────────────────────────────────────────────────────────

// mockLibrary is a deterministic test double for pb.LibraryServer.
// Methods not explicitly overridden fall back to UnimplementedLibraryServer.
type mockLibrary struct {
	pb.UnimplementedLibraryServer
}

func (m *mockLibrary) GetScalars(_ context.Context, _ *pb.GetScalarsRequest) (*pb.GetScalarsResponse, error) {
	// Use values that exercise 64-bit string encoding, enum name, bytes, bool, string.
	return &pb.GetScalarsResponse{
		Scalars: &pb.ScalarTypes{
			FieldInt64:  9007199254740993, // > 2^53: must arrive as string "9007199254740993"
			FieldBool:   true,
			FieldString: "hello-scalars",
			FieldBytes:  []byte{0xde, 0xad, 0xbe, 0xef},
			FieldInt32:  42,
		},
	}, nil
}

func (m *mockLibrary) SearchBooks(_ context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	// Branch on the input oneof: text → Book, otherwise → NotFound.
	if req.GetText() != "" {
		return &pb.SearchResponse{
			Result: &pb.SearchResponse_Book{
				Book: &pb.Book{
					Id:    "book-42",
					Title: "The Go Programming Language",
					Genre: pb.Genre_NONFICTION,
				},
			},
		}, nil
	}
	return &pb.SearchResponse{
		Result: &pb.SearchResponse_NotFound{
			NotFound: &pb.NotFound{Reason: "no books found"},
		},
	}, nil
}

func (m *mockLibrary) AddBook(_ context.Context, req *pb.AddBookRequest) (*pb.AddBookResponse, error) {
	// Echo the book back.
	return &pb.AddBookResponse{Book: req.Book}, nil
}

func (m *mockLibrary) WatchItems(req *pb.WatchRequest, srv pb.Library_WatchItemsServer) error {
	events := []*pb.WatchEvent{
		{Book: &pb.Book{Id: "w1", Title: "Event One"}, At: timestamppb.New(time.Unix(1700000001, 0).UTC())},
		{Book: &pb.Book{Id: "w2", Title: "Event Two"}, At: timestamppb.New(time.Unix(1700000002, 0).UTC())},
		{Book: &pb.Book{Id: "w3", Title: "Event Three"}, At: timestamppb.New(time.Unix(1700000003, 0).UTC())},
	}
	for _, e := range events {
		if err := srv.Send(e); err != nil {
			return err
		}
	}
	return nil
}

// ─── Test server construction ─────────────────────────────────────────────────

func newIntegrationServer() *handler.Server {
	es := exec.NewExecutableSchema(exec.Config{
		Resolvers: &gqlapi.Resolver{Library: &mockLibrary{}},
	})
	srv := handler.New(es)
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	srv.AddTransport(transport.Websocket{
		KeepAlivePingInterval: 10 * time.Second,
	})
	return srv
}

// ─── Real GraphQL HTTP client helper ─────────────────────────────────────────

// gqlPost sends a real GraphQL-over-HTTP POST to url, decodes the response,
// fails the test on GraphQL errors, and unmarshals the "data" field into out.
func gqlPost(t *testing.T, url, query string, vars map[string]any, out any) {
	t.Helper()

	body := map[string]any{"query": query}
	if vars != nil {
		body["variables"] = vars
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("gqlPost: marshal request: %v", err)
	}

	resp, err := http.Post(url, "application/json", bytes.NewReader(b)) //nolint:noctx
	if err != nil {
		t.Fatalf("gqlPost: POST %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gqlPost: HTTP %d from %s", resp.StatusCode, url)
	}

	var result struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("gqlPost: decode response: %v", err)
	}
	if len(result.Errors) > 0 {
		t.Fatalf("gqlPost: GraphQL errors: %s", result.Errors)
	}
	if out != nil {
		if err := json.Unmarshal(result.Data, out); err != nil {
			t.Fatalf("gqlPost: unmarshal data: %v", err)
		}
	}
}

// ─── HTTP integration tests ───────────────────────────────────────────────────

// TestIntegration_GetScalars asserts that int64 > 2^53 arrives as a string
// and that bool/string/bytes fields are correctly wired end-to-end over HTTP.
func TestIntegration_GetScalars(t *testing.T) {
	ts := httptest.NewServer(newIntegrationServer())
	defer ts.Close()

	var data struct {
		GetScalars struct {
			Scalars struct {
				FieldInt64  string `json:"fieldInt64"`
				FieldBool   bool   `json:"fieldBool"`
				FieldString string `json:"fieldString"`
				FieldBytes  string `json:"fieldBytes"` // Bytes scalar → base64 string
				FieldInt32  int    `json:"fieldInt32"`
			} `json:"scalars"`
		} `json:"getScalars"`
	}

	gqlPost(t, ts.URL, `{
		getScalars(input: {id: "test", genre: GENRE_UNSPECIFIED, tags: []}) {
			scalars {
				fieldInt64
				fieldBool
				fieldString
				fieldBytes
				fieldInt32
			}
		}
	}`, nil, &data)

	scalars := data.GetScalars.Scalars

	// 64-bit int must arrive as string (protojson wire form).
	if scalars.FieldInt64 != "9007199254740993" {
		t.Errorf("fieldInt64 = %q, want %q", scalars.FieldInt64, "9007199254740993")
	}
	if !scalars.FieldBool {
		t.Errorf("fieldBool = false, want true")
	}
	if scalars.FieldString != "hello-scalars" {
		t.Errorf("fieldString = %q, want hello-scalars", scalars.FieldString)
	}
	// bytes {0xde,0xad,0xbe,0xef} → base64 "3q2+7w==" (standard encoding).
	if scalars.FieldBytes != "3q2+7w==" {
		t.Errorf("fieldBytes = %q, want 3q2+7w==", scalars.FieldBytes)
	}
	if scalars.FieldInt32 != 42 {
		t.Errorf("fieldInt32 = %d, want 42", scalars.FieldInt32)
	}
}

// TestIntegration_SearchBooks_BookMember asserts the Book union member is
// dispatched correctly when the text oneof variant is set.
func TestIntegration_SearchBooks_BookMember(t *testing.T) {
	ts := httptest.NewServer(newIntegrationServer())
	defer ts.Close()

	var data struct {
		SearchBooks struct {
			Result struct {
				Typename string `json:"__typename"`
				ID       string `json:"id"`
				Title    string `json:"title"`
				Genre    string `json:"genre"`
			} `json:"result"`
		} `json:"searchBooks"`
	}

	gqlPost(t, ts.URL, `{
		searchBooks(input: {query: {text: "golang"}}) {
			result {
				__typename
				... on SearchResponseResultBook {
					id
					title
					genre
				}
				... on SearchResponseResultNotFound {
					reason
				}
			}
		}
	}`, nil, &data)

	r := data.SearchBooks.Result
	if r.Typename != "SearchResponseResultBook" {
		t.Errorf("__typename = %q, want SearchResponseResultBook", r.Typename)
	}
	if r.ID != "book-42" {
		t.Errorf("id = %q, want book-42", r.ID)
	}
	if r.Title != "The Go Programming Language" {
		t.Errorf("title = %q, want 'The Go Programming Language'", r.Title)
	}
	if r.Genre != "NONFICTION" {
		t.Errorf("genre = %q, want NONFICTION", r.Genre)
	}
}

// TestIntegration_SearchBooks_NotFoundMember asserts the NotFound union member
// is dispatched when the author oneof variant is set (text is empty).
func TestIntegration_SearchBooks_NotFoundMember(t *testing.T) {
	ts := httptest.NewServer(newIntegrationServer())
	defer ts.Close()

	var data struct {
		SearchBooks struct {
			Result struct {
				Typename string `json:"__typename"`
				Reason   string `json:"reason"`
			} `json:"result"`
		} `json:"searchBooks"`
	}

	gqlPost(t, ts.URL, `{
		searchBooks(input: {query: {author: "Unknown"}}) {
			result {
				__typename
				... on SearchResponseResultBook {
					id
					title
				}
				... on SearchResponseResultNotFound {
					reason
				}
			}
		}
	}`, nil, &data)

	r := data.SearchBooks.Result
	if r.Typename != "SearchResponseResultNotFound" {
		t.Errorf("__typename = %q, want SearchResponseResultNotFound", r.Typename)
	}
	if r.Reason != "no books found" {
		t.Errorf("reason = %q, want 'no books found'", r.Reason)
	}
}

// TestIntegration_AddBook asserts the addBook mutation echoes the input book.
func TestIntegration_AddBook(t *testing.T) {
	ts := httptest.NewServer(newIntegrationServer())
	defer ts.Close()

	var data struct {
		AddBook struct {
			Book struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Genre string `json:"genre"`
			} `json:"book"`
		} `json:"addBook"`
	}

	gqlPost(t, ts.URL, `mutation {
		addBook(input: {book: {
			id: "new-book-1"
			title: "Clean Code"
			genre: NONFICTION
			copies: "5"
			cover: ""
		}}) {
			book {
				id
				title
				genre
			}
		}
	}`, nil, &data)

	b := data.AddBook.Book
	if b.ID != "new-book-1" {
		t.Errorf("id = %q, want new-book-1", b.ID)
	}
	if b.Title != "Clean Code" {
		t.Errorf("title = %q, want 'Clean Code'", b.Title)
	}
	if b.Genre != "NONFICTION" {
		t.Errorf("genre = %q, want NONFICTION", b.Genre)
	}
}

// ─── Real WebSocket subscription (graphql-transport-ws) ───────────────────────

// wsMsg is the envelope for the graphql-transport-ws protocol.
type wsMsg struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// TestIntegration_WatchItems_Subscription dials the test server over a real
// WebSocket, performs the graphql-transport-ws handshake, subscribes to
// watchItems, and asserts all 3 events arrive in order before "complete".
func TestIntegration_WatchItems_Subscription(t *testing.T) {
	ts := httptest.NewServer(newIntegrationServer())
	defer ts.Close()

	// Convert http:// → ws://
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	dialer := websocket.Dialer{
		Subprotocols: []string{"graphql-transport-ws"},
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket dial: %v", err)
	}
	defer conn.Close()

	// Set an overall read deadline so the test can't hang.
	deadline := time.Now().Add(15 * time.Second)
	if err := conn.SetReadDeadline(deadline); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	send := func(msg wsMsg) {
		t.Helper()
		b, _ := json.Marshal(msg)
		if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
			t.Fatalf("ws send %s: %v", msg.Type, err)
		}
	}

	recv := func() wsMsg {
		t.Helper()
		_, b, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("ws recv: %v", err)
		}
		var m wsMsg
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("ws recv unmarshal: %v", err)
		}
		return m
	}

	// 1. Send connection_init
	send(wsMsg{Type: "connection_init"})

	// 2. Wait for connection_ack (skip any pings or ka messages).
	for {
		m := recv()
		if m.Type == "connection_ack" {
			break
		}
		if m.Type == "ping" {
			send(wsMsg{Type: "pong"})
			continue
		}
		// Ignore other protocol messages (e.g. ka from older protocol).
	}

	// 3. Send subscribe
	payload, _ := json.Marshal(map[string]any{
		"query": `subscription { watchItems(input: {genre: FICTION}) { book { id title } at } }`,
	})
	send(wsMsg{ID: "1", Type: "subscribe", Payload: payload})

	// 4. Collect "next" events; expect "complete" at the end.
	type watchItemsData struct {
		WatchItems struct {
			Book struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"book"`
			At string `json:"at"`
		} `json:"watchItems"`
	}

	wantIDs := []string{"w1", "w2", "w3"}
	var received []watchItemsData

	for {
		m := recv()
		switch m.Type {
		case "next":
			if m.ID != "1" {
				t.Fatalf("unexpected subscription id %q", m.ID)
			}
			var envelope struct {
				Data watchItemsData `json:"data"`
			}
			if err := json.Unmarshal(m.Payload, &envelope); err != nil {
				t.Fatalf("unmarshal next payload: %v", err)
			}
			received = append(received, envelope.Data)
		case "complete":
			if m.ID != "1" {
				t.Fatalf("complete for unexpected id %q", m.ID)
			}
			goto done
		case "ping":
			send(wsMsg{Type: "pong"})
		case "error":
			t.Fatalf("subscription error: %s", m.Payload)
		default:
			// Ignore unknown protocol messages.
			t.Logf("ws: ignoring message type %q", m.Type)
		}
	}

done:
	if len(received) != len(wantIDs) {
		t.Fatalf("got %d events, want %d", len(received), len(wantIDs))
	}
	for i, wantID := range wantIDs {
		gotID := received[i].WatchItems.Book.ID
		if gotID != wantID {
			t.Errorf("event[%d] book.id = %q, want %q", i, gotID, wantID)
		}
	}
	// Verify the At timestamp is non-empty (it's a Timestamp scalar → RFC3339).
	for i, ev := range received {
		if ev.WatchItems.At == "" {
			t.Errorf("event[%d] at is empty, want non-empty RFC3339 timestamp", i)
		}
	}

	// Confirm expected titles.
	wantTitles := []string{"Event One", "Event Two", "Event Three"}
	for i, wantTitle := range wantTitles {
		gotTitle := received[i].WatchItems.Book.Title
		if gotTitle != wantTitle {
			t.Errorf("event[%d] book.title = %q, want %q", i, gotTitle, wantTitle)
		}
	}

}
