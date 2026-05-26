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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeLibrary is a test double for pb.LibraryServer.
type fakeLibrary struct {
	pb.UnimplementedLibraryServer
	book         *pb.Book
	stream       []*pb.Book
	searchResult *pb.SearchResponse // response for SearchBooks
	searchReq    *pb.SearchRequest  // captures the last SearchBooks request
}

func (f *fakeLibrary) GetBook(_ context.Context, _ *pb.GetBookRequest) (*pb.GetBookResponse, error) {
	return &pb.GetBookResponse{Book: f.book}, nil
}

func (f *fakeLibrary) AddBook(_ context.Context, req *pb.AddBookRequest) (*pb.AddBookResponse, error) {
	return &pb.AddBookResponse{Book: req.Book}, nil
}

func (f *fakeLibrary) WatchBooks(_ *pb.WatchBooksRequest, srv pb.Library_WatchBooksServer) error {
	for _, b := range f.stream {
		if err := srv.Send(b); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeLibrary) SearchBooks(_ context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	f.searchReq = req
	if f.searchResult != nil {
		return f.searchResult, nil
	}
	return &pb.SearchResponse{}, nil
}

func newTestServer(lib pb.LibraryServer) *handler.Server {
	return handler.NewDefaultServer(exec.NewExecutableSchema(exec.Config{
		Resolvers: &gqlapi.Resolver{Library: lib},
	}))
}

// TestWireRoundTrip proves the GraphQL output is byte-compatible with protojson
// on the cases that silently corrupt otherwise: 64-bit int (string), enum (name),
// Timestamp (RFC3339), bytes (base64).
func TestWireRoundTrip(t *testing.T) {
	want := &pb.Book{
		Id:          "b1",
		Title:       "T",
		Genre:       pb.Genre_FICTION,
		Copies:      9007199254740993, // > 2^53, loses precision as JSON number
		Cover:       []byte{0x00, 0x01, 0xff},
		PublishedAt: timestamppb.New(time.Unix(1700000000, 123456789).UTC()),
	}
	c := client.New(newTestServer(&fakeLibrary{book: want}))

	var resp struct {
		GetBook struct {
			Book struct {
				ID, Title, Genre, Copies, Cover, PublishedAt string
			}
		}
	}
	c.MustPost(`{ getBook(input:{id:"b1"}) { book { id title genre copies cover publishedAt } } }`, &resp)

	// protojson is the canonical wire form; compare GraphQL output field-by-field.
	pj, err := protojson.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(pj, &m); err != nil {
		t.Fatal(err)
	}
	if resp.GetBook.Book.Copies != m["copies"] {
		t.Fatalf("copies: gql=%q protojson=%v", resp.GetBook.Book.Copies, m["copies"])
	}
	if resp.GetBook.Book.Genre != "FICTION" {
		t.Fatalf("genre = %q, want FICTION", resp.GetBook.Book.Genre)
	}
	if resp.GetBook.Book.Cover != m["cover"] {
		t.Fatalf("cover: gql=%q protojson=%v", resp.GetBook.Book.Cover, m["cover"])
	}
	if resp.GetBook.Book.PublishedAt != m["publishedAt"] {
		t.Fatalf("publishedAt: gql=%q protojson=%v", resp.GetBook.Book.PublishedAt, m["publishedAt"])
	}
}

// TestSearchBooks_OutputUnion_Book proves the output union resolves to the Book member
// when SearchResponse.result is a Book variant.
func TestSearchBooks_OutputUnion_Book(t *testing.T) {
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
	if resp.SearchBooks.Result.Title != "Dune" {
		t.Errorf("title = %q, want Dune", resp.SearchBooks.Result.Title)
	}
}

// TestSearchBooks_OutputUnion_NotFound proves the output union resolves to the NotFound member.
func TestSearchBooks_OutputUnion_NotFound(t *testing.T) {
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

// TestSearchBooks_InputOneof_Text proves that a @oneOf input with text set
// reaches the fake handler correctly mapped to pb.SearchRequest.
func TestSearchBooks_InputOneof_Text(t *testing.T) {
	fake := &fakeLibrary{}
	c := client.New(newTestServer(fake))

	var resp struct {
		SearchBooks struct {
			Result *struct{ Typename string `json:"__typename"` }
		}
	}
	c.MustPost(`{ searchBooks(input:{query:{text:"golang"}}) { result { __typename } } }`, &resp)

	if fake.searchReq == nil {
		t.Fatal("searchReq not captured")
	}
	if txt := fake.searchReq.GetText(); txt != "golang" {
		t.Errorf("text oneof: got %q, want golang", txt)
	}
	if fake.searchReq.GetAuthor() != "" {
		t.Errorf("author should be empty, got %q", fake.searchReq.GetAuthor())
	}
}

// TestSearchBooks_InputOneof_Author proves that a @oneOf input with author set
// reaches the fake handler correctly mapped to pb.SearchRequest.
func TestSearchBooks_InputOneof_Author(t *testing.T) {
	fake := &fakeLibrary{}
	c := client.New(newTestServer(fake))

	var resp struct {
		SearchBooks struct {
			Result *struct{ Typename string `json:"__typename"` }
		}
	}
	c.MustPost(`{ searchBooks(input:{query:{author:"Tolkien"}}) { result { __typename } } }`, &resp)

	if fake.searchReq == nil {
		t.Fatal("searchReq not captured")
	}
	if author := fake.searchReq.GetAuthor(); author != "Tolkien" {
		t.Errorf("author oneof: got %q, want Tolkien", author)
	}
	if fake.searchReq.GetText() != "" {
		t.Errorf("text should be empty, got %q", fake.searchReq.GetText())
	}
}

// TestSubscription proves the server-stream → channel → WS subscription path.
func TestSubscription(t *testing.T) {
	books := []*pb.Book{{Id: "1"}, {Id: "2"}}
	srv := handler.New(exec.NewExecutableSchema(exec.Config{
		Resolvers: &gqlapi.Resolver{Library: &fakeLibrary{stream: books}},
	}))
	srv.AddTransport(transport.Websocket{})
	srv.AddTransport(transport.POST{})

	c := client.New(srv)
	sub := c.Websocket(`subscription { watchBooks(input:{genre:FICTION}) { id } }`)
	defer sub.Close()

	for _, wantID := range []string{"1", "2"} {
		var resp struct {
			WatchBooks struct{ ID string }
		}
		if err := sub.Next(&resp); err != nil {
			t.Fatalf("next: %v", err)
		}
		if resp.WatchBooks.ID != wantID {
			t.Fatalf("got id %q, want %q", resp.WatchBooks.ID, wantID)
		}
	}
}
