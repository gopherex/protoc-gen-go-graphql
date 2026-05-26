package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
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

func TestUint64RoundTrip(t *testing.T) {
	const big uint64 = 18446744073709551615 // max uint64
	var sb strings.Builder
	MarshalUint64(big).MarshalGQL(&sb)
	if got := sb.String(); got != `"18446744073709551615"` {
		t.Fatalf("marshal = %s, want quoted string", got)
	}

	// unmarshal from string
	out, err := UnmarshalUint64("12345")
	if err != nil || out != 12345 {
		t.Fatalf("unmarshal from string = %d, %v", out, err)
	}

	// unmarshal from json.Number
	out, err = UnmarshalUint64(json.Number("12345"))
	if err != nil || out != 12345 {
		t.Fatalf("unmarshal from json.Number = %d, %v", out, err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	v := map[string]any{"a": float64(1)}
	var sb strings.Builder
	MarshalJSON(v).MarshalGQL(&sb)
	got := sb.String()

	// must not have trailing newline
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("MarshalJSON output has trailing newline: %q", got)
	}

	// must be valid JSON
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("MarshalJSON produced invalid JSON %q: %v", got, err)
	}

	// UnmarshalJSON returns input unchanged
	result, err := UnmarshalJSON(v)
	if err != nil {
		t.Fatalf("UnmarshalJSON error: %v", err)
	}
	rm, ok := result.(map[string]any)
	if !ok || rm["a"] != float64(1) {
		t.Fatalf("UnmarshalJSON returned unexpected value: %v", result)
	}
}

func TestTimestampNil(t *testing.T) {
	var sb strings.Builder
	MarshalTimestamp(nil).MarshalGQL(&sb)
	if got := sb.String(); got != "null" {
		t.Fatalf("MarshalTimestamp(nil) = %q, want \"null\"", got)
	}
}

func TestDurationRoundTrip(t *testing.T) {
	d := durationpb.New(1500 * time.Millisecond)
	var sb strings.Builder
	MarshalDuration(d).MarshalGQL(&sb)
	got := sb.String()
	if got != `"1.5s"` {
		t.Fatalf("MarshalDuration = %s, want \"1.5s\"", got)
	}

	back, err := UnmarshalDuration(strings.Trim(got, `"`))
	if err != nil {
		t.Fatalf("UnmarshalDuration error: %v", err)
	}
	if back.AsDuration() != d.AsDuration() {
		t.Fatalf("roundtrip mismatch: got %v, want %v", back.AsDuration(), d.AsDuration())
	}
}

func TestUnmarshalErrors(t *testing.T) {
	// invalid base64
	_, err := UnmarshalBytes("!!!notbase64")
	if err == nil {
		t.Fatal("UnmarshalBytes(invalid base64) expected error, got nil")
	}

	// non-string timestamp
	_, err = UnmarshalTimestamp(123)
	if err == nil {
		t.Fatal("UnmarshalTimestamp(123) expected error, got nil")
	}
}
