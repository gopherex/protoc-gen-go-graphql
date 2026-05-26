package generator

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestScalarForKind(t *testing.T) {
	cases := map[protoreflect.Kind]string{
		protoreflect.StringKind: "String",
		protoreflect.BoolKind:   "Boolean",
		protoreflect.Int32Kind:  "Int",
		protoreflect.Int64Kind:  "Int64",
		protoreflect.Uint64Kind: "Uint64",
		protoreflect.BytesKind:  "Bytes",
		protoreflect.DoubleKind: "Float",
	}
	for k, want := range cases {
		if got := scalarForKind(k); got != want {
			t.Fatalf("kind %v: got %q want %q", k, got, want)
		}
	}
}
