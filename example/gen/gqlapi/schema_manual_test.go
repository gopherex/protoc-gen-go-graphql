package gqlapi

import (
	"context"
	"testing"
	"time"

	"github.com/graphql-go/graphql"
	"google.golang.org/grpc"

	pb "github.com/gopherex/protoc-gen-go-graphql/example/gen"
)

// fakeLib is a minimal LibraryServer used to prove the generated schema
// delegates to a real pb.*ServiceServer (query + subscription) with no transport.
type fakeLib struct {
	pb.UnimplementedLibraryServer
}

func (fakeLib) Ping(ctx context.Context, _ *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{}, nil
}

func (fakeLib) WatchItems(req *pb.WatchRequest, ss grpc.ServerStreamingServer[pb.WatchEvent]) error {
	for _, id := range []string{"b1", "b2"} {
		if err := ss.Send(&pb.WatchEvent{Book: &pb.Book{Id: id}}); err != nil {
			return err
		}
	}
	return nil
}

func newTestSchema(t *testing.T) graphql.Schema {
	t.Helper()
	s, err := NewSchema(&Server{Library: fakeLib{}})
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	return s
}

// TestQueryDelegates proves a Query field invokes the gRPC server impl.
func TestQueryDelegates(t *testing.T) {
	s := newTestSchema(t)
	res := graphql.Do(graphql.Params{Schema: s, RequestString: `{ ping { ok } }`})
	if len(res.Errors) > 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	data, _ := res.Data.(map[string]interface{})
	ping, _ := data["ping"].(map[string]interface{})
	if ping["ok"] != true {
		t.Fatalf("ping.ok = %v, want true (data=%v)", ping["ok"], res.Data)
	}
}

// TestSubscriptionDelegates proves a Subscription field pumps a server-streaming
// RPC into the graphql-go subscription channel.
func TestSubscriptionDelegates(t *testing.T) {
	s := newTestSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch := graphql.Subscribe(graphql.Params{
		Schema:        s,
		RequestString: `subscription { watchItems(genre: FICTION) { book { id } } }`,
		Context:       ctx,
	})
	var ids []string
	for res := range ch {
		if len(res.Errors) > 0 {
			t.Fatalf("subscription errors: %v", res.Errors)
		}
		data, _ := res.Data.(map[string]interface{})
		wi, _ := data["watchItems"].(map[string]interface{})
		book, _ := wi["book"].(map[string]interface{})
		if id, ok := book["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) != 2 || ids[0] != "b1" || ids[1] != "b2" {
		t.Fatalf("subscription ids = %v, want [b1 b2]", ids)
	}
}
