// Package pbgql provides GraphQL marshal/unmarshal adapters for proto enums.
// Each proto enum gets a type alias (so it stays the SAME Go type as the pb
// enum, preserving direct field binding) plus Marshal/Unmarshal funcs that gqlgen
// binds by name. This is the pattern the generator will reproduce per enum.
package pbgql

import (
	"fmt"
	"io"
	"strconv"

	"github.com/99designs/gqlgen/graphql"
	pb "github.com/gopherex/protoc-gen-go-graphql/example/gen"
)

// Genre is an alias of the pb enum, so fields typed pb.Genre bind to it directly.
type Genre = pb.Genre

// MarshalGenre writes the proto enum's value NAME (protojson form), e.g. "FICTION".
func MarshalGenre(g pb.Genre) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_, _ = io.WriteString(w, strconv.Quote(g.String()))
	})
}

// UnmarshalGenre parses a proto enum value name back to the pb enum.
func UnmarshalGenre(v any) (pb.Genre, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("Genre must be a string, got %T", v)
	}
	val, ok := pb.Genre_value[s]
	if !ok {
		return 0, fmt.Errorf("invalid Genre %q", s)
	}
	return pb.Genre(val), nil
}
