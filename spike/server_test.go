package spike_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/99designs/gqlgen/client"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	pb "github.com/gopherex/protoc-gen-go-graphql/example/gen"
	"github.com/gopherex/protoc-gen-go-graphql/spike"
	"github.com/gopherex/protoc-gen-go-graphql/spike/generated"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeLibrary struct {
	pb.UnimplementedLibraryServer
	book   *pb.Book
	stream []*pb.Book
}

func (f *fakeLibrary) GetBook(context.Context, *pb.GetBookRequest) (*pb.GetBookResponse, error) {
	return &pb.GetBookResponse{Book: f.book}, nil
}

func (f *fakeLibrary) WatchBooks(_ *pb.WatchBooksRequest, srv pb.Library_WatchBooksServer) error {
	for _, b := range f.stream {
		if err := srv.Send(b); err != nil {
			return err
		}
	}
	return nil
}

func newServer(lib pb.LibraryServer) *handler.Server {
	return handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{
		Resolvers: &spike.Resolver{Library: lib},
	}))
}

// TestWireRoundTrip proves the GraphQL output is byte-compatible with protojson on
// the cases that silently corrupt otherwise: 64-bit int (string), enum (name),
// Timestamp (RFC3339), bytes (base64).
func TestWireRoundTrip(t *testing.T) {
	want := &pb.Book{
		Id:          "b1",
		Title:       "T",
		Genre:       pb.Genre_FICTION,
		Copies:      9007199254740993, // > 2^53, loses precision as a JSON number
		Cover:       []byte{0x00, 0x01, 0xff},
		PublishedAt: timestamppb.New(time.Unix(1700000000, 123456789).UTC()),
	}
	c := client.New(newServer(&fakeLibrary{book: want}))

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

// TestSubscription proves the server-stream -> channel -> WS subscription path.
func TestSubscription(t *testing.T) {
	books := []*pb.Book{{Id: "1"}, {Id: "2"}}
	srv := handler.New(generated.NewExecutableSchema(generated.Config{
		Resolvers: &spike.Resolver{Library: &fakeLibrary{stream: books}},
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
