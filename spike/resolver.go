package spike

import (
	"context"

	pb "github.com/gopherex/protoc-gen-go-graphql/example/gen"
	"github.com/gopherex/protoc-gen-go-graphql/runtime"
	"github.com/gopherex/protoc-gen-go-graphql/spike/generated"
)

// Resolver delegates GraphQL operations to the gRPC server implementation. It is
// the hand-written reference target the generator must reproduce: one resolver
// type implementing the gqlgen ResolverRoot interface, each method delegating to
// the gRPC server with zero proto<->model conversion.
type Resolver struct {
	Library pb.LibraryServer
}

func (r *Resolver) Book() generated.BookResolver                 { return bookResolver{r} }
func (r *Resolver) Mutation() generated.MutationResolver         { return mutationResolver{r} }
func (r *Resolver) Query() generated.QueryResolver               { return queryResolver{r} }
func (r *Resolver) Subscription() generated.SubscriptionResolver { return subscriptionResolver{r} }

type bookResolver struct{ *Resolver }
type mutationResolver struct{ *Resolver }
type queryResolver struct{ *Resolver }
type subscriptionResolver struct{ *Resolver }

// Tags exposes the proto map<string,string> field as a JSON scalar (field
// resolver, because the JSON scalar's Go type `any` cannot bind a concrete map).
func (r bookResolver) Tags(ctx context.Context, obj *pb.Book) (any, error) {
	return obj.GetTags(), nil
}

func (r queryResolver) GetBook(ctx context.Context, input pb.GetBookRequest) (*pb.GetBookResponse, error) {
	resp, err := r.Library.GetBook(ctx, &input)
	if err != nil {
		return nil, runtime.GraphQLError(ctx, err)
	}
	return resp, nil
}

func (r mutationResolver) AddBook(ctx context.Context, input pb.AddBookRequest) (*pb.AddBookResponse, error) {
	resp, err := r.Library.AddBook(ctx, &input)
	if err != nil {
		return nil, runtime.GraphQLError(ctx, err)
	}
	return resp, nil
}

func (r subscriptionResolver) WatchBooks(ctx context.Context, input pb.WatchBooksRequest) (<-chan *pb.Book, error) {
	return runtime.PumpServerStream[pb.Book](ctx, func(ss *runtime.StreamServer[pb.Book]) error {
		return r.Library.WatchBooks(&input, ss)
	}), nil
}
