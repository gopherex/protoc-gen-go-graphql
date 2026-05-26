package runtime

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/99designs/gqlgen/graphql"
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

func MarshalUInt64(v uint64) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_, _ = io.WriteString(w, strconv.Quote(strconv.FormatUint(v, 10)))
	})
}

func UnmarshalUInt64(v any) (uint64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("UInt64 must be a string, got %T", v)
	}
	return strconv.ParseUint(s, 10, 64)
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

// Timestamp: protojson RFC3339 with nanos, e.g. "2006-01-02T15:04:05.999999999Z".
func MarshalTimestamp(v *timestamppb.Timestamp) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		if v == nil {
			_, _ = io.WriteString(w, "null")
			return
		}
		_, _ = io.WriteString(w, strconv.Quote(v.AsTime().UTC().Format(time.RFC3339Nano)))
	})
}

func UnmarshalTimestamp(v any) (*timestamppb.Timestamp, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("Timestamp must be an RFC3339 string, got %T", v)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, err
	}
	return timestamppb.New(t), nil
}

// Duration: protojson string with trailing "s", e.g. "1.000340012s".
func MarshalDuration(v *durationpb.Duration) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		if v == nil {
			_, _ = io.WriteString(w, "null")
			return
		}
		_, _ = io.WriteString(w, strconv.Quote(v.AsDuration().String()))
	})
}

func UnmarshalDuration(v any) (*durationpb.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("Duration must be a string, got %T", v)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return nil, err
	}
	return durationpb.New(d), nil
}

// JSON: maps, Struct, Value, Any, ListValue. Stored as Go map/any per protojson.
func MarshalJSON(v any) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_ = json.NewEncoder(w).Encode(v)
	})
}

func UnmarshalJSON(v any) (any, error) { return v, nil }
