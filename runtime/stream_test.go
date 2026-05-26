package runtime

import (
	"context"
	"testing"
)

func TestStreamServerSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan *int, 2)
	ss := NewStreamServer[int](ctx, ch)
	v := 7
	if err := ss.Send(&v); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := <-ch; *got != 7 {
		t.Fatalf("got %d", *got)
	}
	cancel()
	w := 8
	if err := ss.Send(&w); err == nil {
		t.Fatalf("send after cancel should error")
	}
}

func TestPumpServerStream(t *testing.T) {
	ctx := context.Background()
	start := func(ss *StreamServer[int]) error {
		for _, n := range []int{1, 2, 3} {
			v := n
			if err := ss.Send(&v); err != nil {
				return err
			}
		}
		return nil
	}
	ch := PumpServerStream[int](ctx, start)
	var got []int
	for v := range ch {
		got = append(got, *v)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("expected [1 2 3], got %v", got)
	}
}
