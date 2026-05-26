package runtime

import (
	"context"

	"google.golang.org/grpc"
)

// StreamServer is a generic gRPC server-streaming shim whose Send pushes into a
// channel. It satisfies any pb `Svc_MethodServer` interface (which embeds
// grpc.ServerStream and adds Send(*T) error) when T is the streamed message.
type StreamServer[T any] struct {
	grpc.ServerStream
	ctx context.Context
	ch  chan<- *T
}

func NewStreamServer[T any](ctx context.Context, ch chan<- *T) *StreamServer[T] {
	return &StreamServer[T]{ctx: ctx, ch: ch}
}

func (s *StreamServer[T]) Context() context.Context { return s.ctx }

func (s *StreamServer[T]) Send(m *T) error {
	// Check cancellation first so a cancelled context deterministically wins over
	// an available channel buffer slot (select picks randomly among ready cases).
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

// PumpServerStream runs a server-streaming gRPC method into a channel returned to
// a gqlgen subscription resolver. `start` invokes the gRPC method with the shim.
func PumpServerStream[T any](ctx context.Context, start func(ss *StreamServer[T]) error) <-chan *T {
	ch := make(chan *T)
	go func() {
		defer close(ch)
		_ = start(NewStreamServer[T](ctx, ch))
	}()
	return ch
}
