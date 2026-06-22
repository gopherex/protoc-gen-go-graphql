package graphqlrt

import (
	"context"

	"google.golang.org/grpc"
)

// StreamServer is a generic in-process gRPC server-streaming shim whose Send
// pushes each message into a channel. By embedding grpc.ServerStream (left nil)
// and adding Send(*T) error + Context(), it satisfies any generated
// `Svc_MethodServer` interface for a server-streaming RPC — letting a generated
// subscription resolver invoke the user's pb.*ServiceServer method directly,
// with no network transport.
type StreamServer[T any] struct {
	grpc.ServerStream
	ctx context.Context
	ch  chan<- interface{}
}

func (s *StreamServer[T]) Context() context.Context { return s.ctx }

func (s *StreamServer[T]) Send(m *T) error {
	// Check cancellation first so a cancelled context deterministically wins over
	// an available channel slot (select picks randomly among ready cases).
	if err := s.ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case s.ch <- m:
		return nil
	}
}

// PumpServerStream bridges a server-streaming gRPC method to a graphql-go
// subscription source channel. `start` invokes the gRPC method with the shim
// (e.g. func(ss *StreamServer[WatchEvent]) error { return srv.WatchItems(req, ss) }).
// Each streamed *T is delivered on the returned channel as the Source for the
// subscription field's Resolve. The goroutine ends (and closes the channel) when
// the method returns or ctx is cancelled. The element type is `chan interface{}`
// because that is the concrete type graphql-go's subscription executor type-switches on.
func PumpServerStream[T any](ctx context.Context, start func(ss *StreamServer[T]) error) chan interface{} {
	ch := make(chan interface{})
	go func() {
		defer close(ch)
		_ = start(&StreamServer[T]{ctx: ctx, ch: ch})
	}()
	return ch
}
