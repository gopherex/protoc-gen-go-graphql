package runtime

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/99designs/gqlgen/graphql"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Int64 (also used for sint64/sfixed64): protojson encodes 64-bit ints as strings.
func MarshalInt64(v int64) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_, _ = io.WriteString(w, strconv.Quote(strconv.FormatInt(v, 10)))
	})
}

func UnmarshalInt64(v any) (int64, error) {
	switch x := v.(type) {
	case string:
		return strconv.ParseInt(x, 10, 64)
	case json.Number:
		return strconv.ParseInt(x.String(), 10, 64)
	case int64:
		return x, nil
	default:
		return 0, fmt.Errorf("Int64 must be a string, got %T", v)
	}
}

func MarshalUint64(v uint64) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_, _ = io.WriteString(w, strconv.Quote(strconv.FormatUint(v, 10)))
	})
}

func UnmarshalUint64(v any) (uint64, error) {
	switch x := v.(type) {
	case string:
		return strconv.ParseUint(x, 10, 64)
	case json.Number:
		return strconv.ParseUint(x.String(), 10, 64)
	case uint64:
		return x, nil
	default:
		return 0, fmt.Errorf("Uint64 must be a string, got %T", v)
	}
}

// Bytes: protojson encodes as standard base64 string.
func MarshalBytes(v []byte) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_, _ = io.WriteString(w, strconv.Quote(base64.StdEncoding.EncodeToString(v)))
	})
}

func UnmarshalBytes(v any) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("Bytes must be a base64 string, got %T", v)
	}
	return base64.StdEncoding.DecodeString(s)
}

// Timestamp: protojson RFC3339, e.g. "2006-01-02T15:04:05.100Z". Uses protojson
// (not time.RFC3339Nano) so fractional seconds are exactly 0/3/6/9 digits,
// byte-identical to protobuf-es. time.RFC3339Nano strips trailing zeros (".1Z")
// which would diverge from protojson (".100Z").
func MarshalTimestamp(v *timestamppb.Timestamp) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		if v == nil {
			_, _ = io.WriteString(w, "null")
			return
		}
		b, err := protojson.Marshal(v)
		if err != nil {
			panic(err)
		}
		_, _ = w.Write(b)
	})
}

func UnmarshalTimestamp(v any) (*timestamppb.Timestamp, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("Timestamp must be an RFC3339 string, got %T", v)
	}
	ts := &timestamppb.Timestamp{}
	if err := protojson.Unmarshal([]byte(strconv.Quote(s)), ts); err != nil {
		return nil, err
	}
	return ts, nil
}

// Duration: protojson string with trailing "s", e.g. "1.000340012s".
func MarshalDuration(v *durationpb.Duration) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		if v == nil {
			_, _ = io.WriteString(w, "null")
			return
		}
		// protojson renders Duration in the canonical proto3-JSON "<seconds>s"
		// form (e.g. "61s", "1.500s") that protobuf-es toJson/fromJson agree on.
		// time.Duration.String() would emit "1m1s" for >= 60s, which is wrong.
		b, err := protojson.Marshal(v)
		if err != nil {
			panic(err)
		}
		_, _ = w.Write(b)
	})
}

func UnmarshalDuration(v any) (*durationpb.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("Duration must be a string, got %T", v)
	}
	d := &durationpb.Duration{}
	if err := protojson.Unmarshal([]byte(strconv.Quote(s)), d); err != nil {
		return nil, err
	}
	return d, nil
}

// JSON: maps, Struct, Value, Any, ListValue. Stored as Go map/any per protojson.
func MarshalJSON(v any) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		_, _ = w.Write(b)
	})
}

// UnmarshalJSON passes the decoded JSON value through unchanged. The caller
// receives whatever the JSON decoder produced (map[string]any, []any, string,
// bool, or json.Number).
func UnmarshalJSON(v any) (any, error) { return v, nil }
