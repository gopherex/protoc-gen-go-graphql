package runtime

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestInt64RoundTrip(t *testing.T) {
	const big int64 = 9007199254740993 // > 2^53, loses precision as JSON number
	m := MarshalInt64(big)
	var sb strings.Builder
	m.MarshalGQL(&sb)
	if got := sb.String(); got != `"9007199254740993"` {
		t.Fatalf("marshal = %s, want quoted string", got)
	}
	out, err := UnmarshalInt64("9007199254740993")
	if err != nil || out != big {
		t.Fatalf("unmarshal = %d, %v", out, err)
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	ts := timestamppb.New(time.Unix(1700000000, 123456789).UTC())
	var sb strings.Builder
	MarshalTimestamp(ts).MarshalGQL(&sb)
	got := strings.Trim(sb.String(), `"`)
	back, err := UnmarshalTimestamp(got)
	if err != nil || !back.AsTime().Equal(ts.AsTime()) {
		t.Fatalf("roundtrip mismatch: %v / %v", back, err)
	}
}

func TestBytesRoundTrip(t *testing.T) {
	in := []byte{0x00, 0x01, 0xff}
	var sb strings.Builder
	MarshalBytes(in).MarshalGQL(&sb)
	out, err := UnmarshalBytes(strings.Trim(sb.String(), `"`))
	if err != nil || string(out) != string(in) {
		t.Fatalf("roundtrip mismatch: %v / %v", out, err)
	}
}
