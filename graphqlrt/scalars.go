// Package graphqlrt is the runtime support library for the graphql-go backend of
// protoc-gen-go-graphql. It provides protojson-aligned custom scalars, a
// gRPC-server-stream→subscription pump, and gRPC-status→GraphQL error mapping.
//
// The generated code (single-pass, no gqlgen) builds a *graphql.Schema whose
// field resolvers delegate directly to the user's pb.*ServiceServer
// implementations; this package supplies the non-generated primitives those
// resolvers and the schema reference.
package graphqlrt

import (
	"encoding/base64"
	"fmt"
	"strconv"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// litString extracts a string from a GraphQL AST literal (StringValue), or "" + false.
func litString(v ast.Value) (string, bool) {
	if sv, ok := v.(*ast.StringValue); ok {
		return sv.Value, true
	}
	return "", false
}

// Int64 maps proto int64/sint64/sfixed64. protojson encodes 64-bit ints as
// decimal strings, so the scalar serializes to / parses from a string.
var Int64 = graphql.NewScalar(graphql.ScalarConfig{
	Name:        "Int64",
	Description: "64-bit signed integer, serialized as a decimal string (protojson-aligned).",
	Serialize: func(value interface{}) interface{} {
		switch v := value.(type) {
		case int64:
			return strconv.FormatInt(v, 10)
		case *int64:
			if v == nil {
				return nil
			}
			return strconv.FormatInt(*v, 10)
		default:
			return nil
		}
	},
	ParseValue: func(value interface{}) interface{} {
		switch v := value.(type) {
		case string:
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil
			}
			return n
		case int:
			return int64(v)
		default:
			return nil
		}
	},
	ParseLiteral: func(valueAST ast.Value) interface{} {
		if s, ok := litString(valueAST); ok {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n
			}
		}
		if iv, ok := valueAST.(*ast.IntValue); ok {
			if n, err := strconv.ParseInt(iv.Value, 10, 64); err == nil {
				return n
			}
		}
		return nil
	},
})

// Uint64 maps proto uint64/fixed64 (decimal string, protojson-aligned).
var Uint64 = graphql.NewScalar(graphql.ScalarConfig{
	Name:        "Uint64",
	Description: "64-bit unsigned integer, serialized as a decimal string (protojson-aligned).",
	Serialize: func(value interface{}) interface{} {
		switch v := value.(type) {
		case uint64:
			return strconv.FormatUint(v, 10)
		case *uint64:
			if v == nil {
				return nil
			}
			return strconv.FormatUint(*v, 10)
		default:
			return nil
		}
	},
	ParseValue: func(value interface{}) interface{} {
		if s, ok := value.(string); ok {
			if n, err := strconv.ParseUint(s, 10, 64); err == nil {
				return n
			}
		}
		return nil
	},
	ParseLiteral: func(valueAST ast.Value) interface{} {
		if s, ok := litString(valueAST); ok {
			if n, err := strconv.ParseUint(s, 10, 64); err == nil {
				return n
			}
		}
		if iv, ok := valueAST.(*ast.IntValue); ok {
			if n, err := strconv.ParseUint(iv.Value, 10, 64); err == nil {
				return n
			}
		}
		return nil
	},
})

// Bytes maps proto bytes as a standard-base64 string (protojson-aligned).
var Bytes = graphql.NewScalar(graphql.ScalarConfig{
	Name:        "Bytes",
	Description: "Binary data, serialized as a standard base64 string (protojson-aligned).",
	Serialize: func(value interface{}) interface{} {
		switch v := value.(type) {
		case []byte:
			return base64.StdEncoding.EncodeToString(v)
		default:
			return nil
		}
	},
	ParseValue: func(value interface{}) interface{} {
		if s, ok := value.(string); ok {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				return b
			}
		}
		return nil
	},
	ParseLiteral: func(valueAST ast.Value) interface{} {
		if s, ok := litString(valueAST); ok {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				return b
			}
		}
		return nil
	},
})

// Timestamp maps google.protobuf.Timestamp as an RFC3339 string via protojson
// (fractional seconds 0/3/6/9 digits, byte-identical to protobuf-es).
var Timestamp = graphql.NewScalar(graphql.ScalarConfig{
	Name:        "Timestamp",
	Description: "RFC 3339 timestamp (protojson-aligned).",
	Serialize: func(value interface{}) interface{} {
		ts, ok := value.(*timestamppb.Timestamp)
		if !ok || ts == nil {
			return nil
		}
		b, err := protojson.Marshal(ts)
		if err != nil {
			return nil
		}
		// protojson wraps the value in quotes; strip them for the Go string value.
		return trimJSONString(b)
	},
	ParseValue:   parseTimestamp,
	ParseLiteral: func(v ast.Value) interface{} { s, _ := litString(v); return parseTimestamp(s) },
})

func parseTimestamp(value interface{}) interface{} {
	s, ok := value.(string)
	if !ok || s == "" {
		return nil
	}
	var ts timestamppb.Timestamp
	if err := protojson.Unmarshal([]byte(strconv.Quote(s)), &ts); err != nil {
		return nil
	}
	return &ts
}

// Duration maps google.protobuf.Duration (canonical proto3-JSON form, e.g. "1.5s").
var Duration = graphql.NewScalar(graphql.ScalarConfig{
	Name:        "Duration",
	Description: "Duration in proto3-JSON form, e.g. \"1.5s\" (protojson-aligned).",
	Serialize: func(value interface{}) interface{} {
		d, ok := value.(*durationpb.Duration)
		if !ok || d == nil {
			return nil
		}
		b, err := protojson.Marshal(d)
		if err != nil {
			return nil
		}
		return trimJSONString(b)
	},
	ParseValue:   parseDuration,
	ParseLiteral: func(v ast.Value) interface{} { s, _ := litString(v); return parseDuration(s) },
})

func parseDuration(value interface{}) interface{} {
	s, ok := value.(string)
	if !ok || s == "" {
		return nil
	}
	var d durationpb.Duration
	if err := protojson.Unmarshal([]byte(strconv.Quote(s)), &d); err != nil {
		return nil
	}
	return &d
}

// JSON is a pass-through scalar for google.protobuf.Struct/Value/Any/ListValue and
// map fields: the resolver yields an already-decoded Go value (map/slice/scalar).
var JSON = graphql.NewScalar(graphql.ScalarConfig{
	Name:         "JSON",
	Description:  "Arbitrary JSON value (objects, arrays, scalars).",
	Serialize:    func(value interface{}) interface{} { return value },
	ParseValue:   func(value interface{}) interface{} { return value },
	ParseLiteral: parseJSONLiteral,
})

// parseJSONLiteral recursively converts a GraphQL AST literal into a plain Go value.
func parseJSONLiteral(valueAST ast.Value) interface{} {
	switch v := valueAST.(type) {
	case *ast.StringValue:
		return v.Value
	case *ast.BooleanValue:
		return v.Value
	case *ast.IntValue:
		if n, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
			return n
		}
		return nil
	case *ast.FloatValue:
		if f, err := strconv.ParseFloat(v.Value, 64); err == nil {
			return f
		}
		return nil
	case *ast.ObjectValue:
		out := map[string]interface{}{}
		for _, f := range v.Fields {
			out[f.Name.Value] = parseJSONLiteral(f.Value)
		}
		return out
	case *ast.ListValue:
		out := make([]interface{}, 0, len(v.Values))
		for _, item := range v.Values {
			out = append(out, parseJSONLiteral(item))
		}
		return out
	default:
		return nil
	}
}

// trimJSONString drops surrounding double-quotes from a protojson-marshaled scalar
// (protojson renders Timestamp/Duration as a quoted JSON string).
func trimJSONString(b []byte) string {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		unq, err := strconv.Unquote(s)
		if err == nil {
			return unq
		}
	}
	return s
}

// MustString is a small helper for resolvers that need to assert a string arg.
func MustString(v interface{}) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("expected string, got %T", v)
}
