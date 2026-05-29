package example_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/99designs/gqlgen/client"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	pb "github.com/gopherex/protoc-gen-go-graphql/example/gen"
	"github.com/gopherex/protoc-gen-go-graphql/example/gen/gqlapi"
	"github.com/gopherex/protoc-gen-go-graphql/example/gen/gqlapi/exec"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakeLibrary is a test double for pb.LibraryServer.
type fakeLibrary struct {
	pb.UnimplementedLibraryServer
	everythingResp *pb.GetEverythingResponse
	scalarsResp    *pb.GetScalarsResponse
	searchResult   *pb.SearchResponse
	searchReq      *pb.SearchRequest
	echoBook       *pb.Book
	watchEvents    []*pb.WatchEvent
}

func (f *fakeLibrary) GetEverything(_ context.Context, _ *pb.GetEverythingRequest) (*pb.GetEverythingResponse, error) {
	if f.everythingResp != nil {
		return f.everythingResp, nil
	}
	return &pb.GetEverythingResponse{}, nil
}

func (f *fakeLibrary) GetScalars(_ context.Context, _ *pb.GetScalarsRequest) (*pb.GetScalarsResponse, error) {
	if f.scalarsResp != nil {
		return f.scalarsResp, nil
	}
	return &pb.GetScalarsResponse{}, nil
}

func (f *fakeLibrary) SearchBooks(_ context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	f.searchReq = req
	if f.searchResult != nil {
		return f.searchResult, nil
	}
	return &pb.SearchResponse{}, nil
}

func (f *fakeLibrary) EchoInput(_ context.Context, req *pb.EchoRequest) (*pb.EchoResponse, error) {
	if req.Book != nil {
		return &pb.EchoResponse{Book: req.Book}, nil
	}
	if f.echoBook != nil {
		return &pb.EchoResponse{Book: f.echoBook}, nil
	}
	return &pb.EchoResponse{}, nil
}

func (f *fakeLibrary) AddBook(_ context.Context, req *pb.AddBookRequest) (*pb.AddBookResponse, error) {
	return &pb.AddBookResponse{Book: req.Book}, nil
}

func (f *fakeLibrary) Ping(_ context.Context, _ *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{}, nil
}

func (f *fakeLibrary) WatchItems(_ *pb.WatchRequest, srv pb.Library_WatchItemsServer) error {
	for _, e := range f.watchEvents {
		if err := srv.Send(e); err != nil {
			return err
		}
	}
	return nil
}

// streamServer is a test helper that satisfies pb.Library_WatchItemsServer.
type streamServer struct {
	grpc.ServerStream
	sent []*pb.WatchEvent
}

func (s *streamServer) Send(e *pb.WatchEvent) error {
	s.sent = append(s.sent, e)
	return nil
}

func newTestServer(lib pb.LibraryServer) *handler.Server {
	return handler.NewDefaultServer(exec.NewExecutableSchema(exec.Config{
		Resolvers: &gqlapi.Resolver{Library: lib},
	}))
}

// ─── Wire round-trip tests ────────────────────────────────────────────────────

// TestWireRoundTrip_Scalars proves that 64-bit int (string), enum (name),
// bytes (base64), and Timestamp (RFC3339) are byte-compatible with protojson.
func TestWireRoundTrip_Scalars(t *testing.T) {
	want := &pb.Book{
		Id:          "b1",
		Title:       "T",
		Genre:       pb.Genre_FICTION,
		Copies:      9007199254740993, // > 2^53, loses precision as JSON number
		Cover:       []byte{0x00, 0x01, 0xff},
		PublishedAt: timestamppb.New(time.Unix(1700000000, 123456789).UTC()),
	}
	// Use EchoInput to test Book round-trip.
	c := client.New(newTestServer(&fakeLibrary{echoBook: want}))

	var resp struct {
		EchoInput struct {
			Book struct {
				ID, Title, Genre, Copies, Cover, PublishedAt string
			}
		}
	}
	// cover: []byte{0x00,0x01,0xff} → base64 "AAH/" (standard encoding, no padding needed for 3 bytes).
	c.MustPost(`mutation { echoInput(input:{genre:GENRE_UNSPECIFIED,book:{id:"b1",title:"T",genre:FICTION,copies:"9007199254740993",cover:"AAH/",publishedAt:"2023-11-14T22:13:20.123456789Z"}}) { book { id title genre copies cover publishedAt } } }`, &resp)

	pj, err := protojson.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(pj, &m); err != nil {
		t.Fatal(err)
	}
	if resp.EchoInput.Book.Copies != m["copies"] {
		t.Fatalf("copies: gql=%q protojson=%v", resp.EchoInput.Book.Copies, m["copies"])
	}
	if resp.EchoInput.Book.Genre != "FICTION" {
		t.Fatalf("genre = %q, want FICTION", resp.EchoInput.Book.Genre)
	}
	if resp.EchoInput.Book.Cover != m["cover"] {
		t.Fatalf("cover: gql=%q protojson=%v", resp.EchoInput.Book.Cover, m["cover"])
	}
	if resp.EchoInput.Book.PublishedAt != m["publishedAt"] {
		t.Fatalf("publishedAt: gql=%q protojson=%v", resp.EchoInput.Book.PublishedAt, m["publishedAt"])
	}
}

// TestWireRoundTrip_Timestamp proves Timestamp with trailing-zero nanos.
func TestWireRoundTrip_Timestamp(t *testing.T) {
	// 100ms = 100_000_000 nanos — protojson emits ".100Z", not ".1Z".
	ts := timestamppb.New(time.Unix(1700000000, 100000000).UTC())
	wantPb := &pb.WatchEvent{At: ts}
	fake := &fakeLibrary{watchEvents: []*pb.WatchEvent{wantPb}}

	srv := handler.New(exec.NewExecutableSchema(exec.Config{
		Resolvers: &gqlapi.Resolver{Library: fake},
	}))
	srv.AddTransport(transport.Websocket{})
	srv.AddTransport(transport.POST{})

	c := client.New(srv)
	sub := c.Websocket(`subscription { watchItems(input:{genre:FICTION}) { at } }`)
	defer sub.Close()

	var resp struct {
		WatchItems struct{ At string }
	}
	if err := sub.Next(&resp); err != nil {
		t.Fatalf("next: %v", err)
	}

	pj, _ := protojson.Marshal(ts)
	// protojson wraps the timestamp as a quoted RFC3339 string.
	// The GQL scalar value and protojson should both be the same RFC3339.
	wantAt := string(pj)               // e.g. `"2023-11-14T22:13:20.100Z"`
	wantAt = wantAt[1 : len(wantAt)-1] // strip quotes
	if resp.WatchItems.At != wantAt {
		t.Fatalf("at: gql=%q, want %q", resp.WatchItems.At, wantAt)
	}
}

// TestWireRoundTrip_RepeatedScalar proves a repeated int64 field round-trips.
func TestWireRoundTrip_RepeatedScalar(t *testing.T) {
	fake := &fakeLibrary{
		scalarsResp: &pb.GetScalarsResponse{
			Repeateds: &pb.RepeatedScalars{
				FieldInt64:  []int64{1, 9007199254740993},
				FieldString: []string{"a", "b"},
			},
		},
	}
	c := client.New(newTestServer(fake))

	var resp struct {
		FetchScalars struct {
			Repeateds struct {
				FieldInt64  []string
				FieldString []string
			}
		}
	}
	c.MustPost(`{ fetchScalars(input:{id:"x",genre:GENRE_UNSPECIFIED,tags:[]}) { repeateds { fieldInt64 fieldString } } }`, &resp)

	if len(resp.FetchScalars.Repeateds.FieldInt64) != 2 {
		t.Fatalf("fieldInt64 len = %d, want 2", len(resp.FetchScalars.Repeateds.FieldInt64))
	}
	if resp.FetchScalars.Repeateds.FieldInt64[1] != "9007199254740993" {
		t.Fatalf("fieldInt64[1] = %q, want 9007199254740993", resp.FetchScalars.Repeateds.FieldInt64[1])
	}
	if len(resp.FetchScalars.Repeateds.FieldString) != 2 {
		t.Fatalf("fieldString len = %d, want 2", len(resp.FetchScalars.Repeateds.FieldString))
	}
}

// TestWireRoundTrip_OptionalScalar proves optional scalars present and absent.
func TestWireRoundTrip_OptionalScalar(t *testing.T) {
	s := "hello"
	fake := &fakeLibrary{
		scalarsResp: &pb.GetScalarsResponse{
			Optionals: &pb.OptionalScalars{
				FieldString: &s,
				// FieldInt64 not set → null
			},
		},
	}
	c := client.New(newTestServer(fake))

	var resp struct {
		FetchScalars struct {
			Optionals struct {
				FieldString *string
				FieldInt64  *string
			}
		}
	}
	c.MustPost(`{ fetchScalars(input:{id:"x",genre:GENRE_UNSPECIFIED,tags:[]}) { optionals { fieldString fieldInt64 } } }`, &resp)

	if resp.FetchScalars.Optionals.FieldString == nil || *resp.FetchScalars.Optionals.FieldString != "hello" {
		t.Fatalf("fieldString = %v, want hello", resp.FetchScalars.Optionals.FieldString)
	}
	if resp.FetchScalars.Optionals.FieldInt64 != nil {
		t.Fatalf("fieldInt64 should be nil, got %v", *resp.FetchScalars.Optionals.FieldInt64)
	}
}

// TestWireRoundTrip_WrapperInt64 proves Int64Value nullable scalar.
func TestWireRoundTrip_WrapperInt64(t *testing.T) {
	fake := &fakeLibrary{
		everythingResp: &pb.GetEverythingResponse{
			Everything: &pb.Everything{
				Wkt: &pb.WKTMessage{
					Int64Wrapper: wrapperspb.Int64(9007199254740993),
				},
			},
		},
	}
	c := client.New(newTestServer(fake))

	var resp struct {
		GetEverything struct {
			Everything struct {
				Wkt struct {
					Int64Wrapper string
				}
			}
		}
	}
	c.MustPost(`{ getEverything(input:{id:"x"}) { everything { wkt { int64Wrapper } } } }`, &resp)

	// protojson represents Int64Value as a quoted string (same as int64 scalar).
	// gqlgen JSON-decodes the scalar, so the client receives the inner string value.
	if resp.GetEverything.Everything.Wkt.Int64Wrapper != "9007199254740993" {
		t.Fatalf("int64Wrapper = %q, want 9007199254740993", resp.GetEverything.Everything.Wkt.Int64Wrapper)
	}
}

// TestWireRoundTrip_Map proves map fields round-trip as JSON.
func TestWireRoundTrip_Map(t *testing.T) {
	fake := &fakeLibrary{
		everythingResp: &pb.GetEverythingResponse{
			Everything: &pb.Everything{
				Maps: &pb.MapMessage{
					StringMap: map[string]string{"key": "val"},
				},
			},
		},
	}
	c := client.New(newTestServer(fake))

	var resp struct {
		GetEverything struct {
			Everything struct {
				Maps struct {
					StringMap map[string]any
				}
			}
		}
	}
	c.MustPost(`{ getEverything(input:{id:"x"}) { everything { maps { stringMap } } } }`, &resp)

	if v, ok := resp.GetEverything.Everything.Maps.StringMap["key"]; !ok || v != "val" {
		t.Fatalf("stringMap[key] = %v, want val", v)
	}
}

// TestWireRoundTrip_DeepNested proves deep nested message access.
func TestWireRoundTrip_DeepNested(t *testing.T) {
	fake := &fakeLibrary{
		everythingResp: &pb.GetEverythingResponse{
			Everything: &pb.Everything{
				Outer: &pb.Outer{
					Tag: "outer-tag",
					Inner: &pb.Outer_Inner{
						Label: "inner-label",
						Deep: &pb.Outer_Inner_DeepInner{
							Value: "deep-value",
						},
					},
				},
			},
		},
	}
	c := client.New(newTestServer(fake))

	var resp struct {
		GetEverything struct {
			Everything struct {
				Outer struct {
					Tag   string
					Inner struct {
						Label string
						Deep  struct {
							Value string
						}
					}
				}
			}
		}
	}
	c.MustPost(`{ getEverything(input:{id:"x"}) { everything { outer { tag inner { label deep { value } } } } } }`, &resp)

	if resp.GetEverything.Everything.Outer.Tag != "outer-tag" {
		t.Fatalf("tag = %q, want outer-tag", resp.GetEverything.Everything.Outer.Tag)
	}
	if resp.GetEverything.Everything.Outer.Inner.Label != "inner-label" {
		t.Fatalf("inner.label = %q, want inner-label", resp.GetEverything.Everything.Outer.Inner.Label)
	}
	if resp.GetEverything.Everything.Outer.Inner.Deep.Value != "deep-value" {
		t.Fatalf("inner.deep.value = %q, want deep-value", resp.GetEverything.Everything.Outer.Inner.Deep.Value)
	}
}

// TestWireRoundTrip_Oneof proves output union dispatch.
func TestWireRoundTrip_Oneof_OutputUnion_Book(t *testing.T) {
	book := &pb.Book{Id: "b1", Title: "Dune"}
	fake := &fakeLibrary{
		searchResult: &pb.SearchResponse{
			Result: &pb.SearchResponse_Book{Book: book},
		},
	}
	c := client.New(newTestServer(fake))

	var resp struct {
		SearchBooks struct {
			Result struct {
				Typename string `json:"__typename"`
				Id       string `json:"id"`
				Title    string `json:"title"`
			}
		}
	}
	c.MustPost(`{
		searchBooks(input:{query:{text:"Dune"}}) {
			result { __typename ... on SearchResponseResultBook { id title } }
		}
	}`, &resp)

	if resp.SearchBooks.Result.Typename != "SearchResponseResultBook" {
		t.Errorf("__typename = %q, want SearchResponseResultBook", resp.SearchBooks.Result.Typename)
	}
	if resp.SearchBooks.Result.Id != "b1" {
		t.Errorf("id = %q, want b1", resp.SearchBooks.Result.Id)
	}
}

// TestWireRoundTrip_Oneof_OutputUnion_NotFound proves the NotFound union member.
func TestWireRoundTrip_Oneof_OutputUnion_NotFound(t *testing.T) {
	fake := &fakeLibrary{
		searchResult: &pb.SearchResponse{
			Result: &pb.SearchResponse_NotFound{NotFound: &pb.NotFound{Reason: "no match"}},
		},
	}
	c := client.New(newTestServer(fake))

	var resp struct {
		SearchBooks struct {
			Result struct {
				Typename string `json:"__typename"`
				Reason   string `json:"reason"`
			}
		}
	}
	c.MustPost(`{
		searchBooks(input:{query:{author:"Unknown"}}) {
			result { __typename ... on SearchResponseResultNotFound { reason } }
		}
	}`, &resp)

	if resp.SearchBooks.Result.Typename != "SearchResponseResultNotFound" {
		t.Errorf("__typename = %q, want SearchResponseResultNotFound", resp.SearchBooks.Result.Typename)
	}
	if resp.SearchBooks.Result.Reason != "no match" {
		t.Errorf("reason = %q, want 'no match'", resp.SearchBooks.Result.Reason)
	}
}

// TestWireRoundTrip_Oneof_Input_Text proves @oneOf input with text.
func TestWireRoundTrip_Oneof_Input_Text(t *testing.T) {
	fake := &fakeLibrary{}
	c := client.New(newTestServer(fake))

	var resp struct {
		SearchBooks struct {
			Result *struct {
				Typename string `json:"__typename"`
			}
		}
	}
	c.MustPost(`{ searchBooks(input:{query:{text:"golang"}}) { result { __typename } } }`, &resp)

	if fake.searchReq == nil {
		t.Fatal("searchReq not captured")
	}
	if txt := fake.searchReq.GetText(); txt != "golang" {
		t.Errorf("text oneof: got %q, want golang", txt)
	}
}

// TestWireRoundTrip_Oneof_Input_Author proves @oneOf input with author.
func TestWireRoundTrip_Oneof_Input_Author(t *testing.T) {
	fake := &fakeLibrary{}
	c := client.New(newTestServer(fake))

	var resp struct {
		SearchBooks struct {
			Result *struct {
				Typename string `json:"__typename"`
			}
		}
	}
	c.MustPost(`{ searchBooks(input:{query:{author:"Tolkien"}}) { result { __typename } } }`, &resp)

	if fake.searchReq == nil {
		t.Fatal("searchReq not captured")
	}
	if author := fake.searchReq.GetAuthor(); author != "Tolkien" {
		t.Errorf("author oneof: got %q, want Tolkien", author)
	}
}

// TestWireRoundTrip_Recursive proves a recursive message (one level).
func TestWireRoundTrip_Recursive(t *testing.T) {
	fake := &fakeLibrary{
		everythingResp: &pb.GetEverythingResponse{
			Everything: &pb.Everything{
				Tree: &pb.TreeNode{
					Id: "root",
					Children: []*pb.TreeNode{
						{Id: "child1"},
					},
				},
				Recursive: []*pb.Everything{
					{Tree: &pb.TreeNode{Id: "nested-root"}},
				},
			},
		},
	}
	c := client.New(newTestServer(fake))

	var resp struct {
		GetEverything struct {
			Everything struct {
				Tree struct {
					Id       string
					Children []struct{ Id string }
				}
				Recursive []struct {
					Tree struct{ Id string }
				}
			}
		}
	}
	c.MustPost(`{ getEverything(input:{id:"x"}) {
		everything {
			tree { id children { id } }
			recursive { tree { id } }
		}
	} }`, &resp)

	if resp.GetEverything.Everything.Tree.Id != "root" {
		t.Fatalf("tree.id = %q, want root", resp.GetEverything.Everything.Tree.Id)
	}
	if len(resp.GetEverything.Everything.Tree.Children) != 1 {
		t.Fatalf("children len = %d, want 1", len(resp.GetEverything.Everything.Tree.Children))
	}
	if resp.GetEverything.Everything.Tree.Children[0].Id != "child1" {
		t.Fatalf("children[0].id = %q, want child1", resp.GetEverything.Everything.Tree.Children[0].Id)
	}
	if len(resp.GetEverything.Everything.Recursive) != 1 {
		t.Fatalf("recursive len = %d, want 1", len(resp.GetEverything.Everything.Recursive))
	}
	if resp.GetEverything.Everything.Recursive[0].Tree.Id != "nested-root" {
		t.Fatalf("recursive[0].tree.id = %q, want nested-root", resp.GetEverything.Everything.Recursive[0].Tree.Id)
	}
}

// TestSubscription proves the server-stream → channel → WS subscription path.
func TestSubscription(t *testing.T) {
	events := []*pb.WatchEvent{{Book: &pb.Book{Id: "1"}}, {Book: &pb.Book{Id: "2"}}}
	srv := handler.New(exec.NewExecutableSchema(exec.Config{
		Resolvers: &gqlapi.Resolver{Library: &fakeLibrary{watchEvents: events}},
	}))
	srv.AddTransport(transport.Websocket{})
	srv.AddTransport(transport.POST{})

	c := client.New(srv)
	sub := c.Websocket(`subscription { watchItems(input:{genre:FICTION}) { book { id } } }`)
	defer sub.Close()

	for _, wantID := range []string{"1", "2"} {
		var resp struct {
			WatchItems struct {
				Book struct{ ID string }
			}
		}
		if err := sub.Next(&resp); err != nil {
			t.Fatalf("next: %v", err)
		}
		if resp.WatchItems.Book.ID != wantID {
			t.Fatalf("got id %q, want %q", resp.WatchItems.Book.ID, wantID)
		}
	}
}
