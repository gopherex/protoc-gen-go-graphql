package generator

import "google.golang.org/protobuf/reflect/protoreflect"

// scalarForKind maps a scalar proto kind to a GraphQL scalar name.
// Message/enum/group kinds are handled by the caller (named types).
// Unsigned 64-bit kinds map to "Uint64" (bound to runtime.MarshalUint64/UnmarshalUint64),
// distinct from signed 64-bit kinds which map to "Int64".
func scalarForKind(k protoreflect.Kind) string {
	switch k {
	case protoreflect.BoolKind:
		return "Boolean"
	case protoreflect.StringKind:
		return "String"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "Int"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "Int64"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "Uint64"
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "Float"
	case protoreflect.BytesKind:
		return "Bytes"
	default:
		return ""
	}
}
