package generator

import (
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// needsForceResolver returns true when the field's Go type is incompatible with
// gqlgen's default binding for its GraphQL scalar type.
//
// Specifically:
//   - float32 fields map to the "Float" scalar, but gqlgen's Float is float64.
//     gqlgen generates a field resolver for the mismatch — we force it explicitly.
//   - uint32 / fixed32 fields map to the "Int" scalar, but gqlgen's Int is int32.
//
// By emitting @goField(forceResolver: true) for these fields, the generator
// controls the resolver signature (returning the compatible scalar type) instead of
// relying on gqlgen's auto-detection. The resolver methods are emitted in resolvers.go.
// wktJSONTypes is the set of WKT FQNs that map to the JSON scalar and need field resolvers.
// gqlgen cannot bind `any` to `*Struct`, `*Value`, etc. directly.
var wktJSONTypes = map[string]bool{
	"google.protobuf.Struct":    true,
	"google.protobuf.Value":     true,
	"google.protobuf.ListValue": true,
	"google.protobuf.Any":       true,
	"google.protobuf.Empty":     true,
}

func needsForceResolver(field *protogen.Field) bool {
	if field.Desc.IsMap() {
		return false
	}
	switch field.Desc.Kind() {
	case protoreflect.FloatKind:
		return true
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return true
	case protoreflect.MessageKind, protoreflect.GroupKind:
		fqn := string(field.Desc.Message().FullName())
		return wktJSONTypes[fqn]
	default:
		return false
	}
}

// pbElemType returns the Go proto element type name for a repeated incompatible field.
// Used in the resolver to convert []pbType → []retType.
func pbElemType(field *protogen.Field) string {
	switch field.Desc.Kind() {
	case protoreflect.FloatKind:
		return "float32"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	default:
		return "any"
	}
}

// wktGoType returns the fully qualified Go type name for a WKT JSON type.
func wktGoType(fqn string) string {
	switch fqn {
	case "google.protobuf.Any":
		return "*anypb.Any"
	case "google.protobuf.Empty":
		return "*emptypb.Empty"
	case "google.protobuf.Struct":
		return "*structpb.Struct"
	case "google.protobuf.Value":
		return "*structpb.Value"
	case "google.protobuf.ListValue":
		return "*structpb.ListValue"
	default:
		return "proto.Message"
	}
}

// coerceReturnType returns the Go type that the resolver method should return
// for a field that needs @goField(forceResolver: true) due to type incompatibility.
// float32 → float64 (gqlgen's Float is float64)
// uint32 / fixed32 → int (gqlgen's Int is int32, but resolver returns int)
// WKT JSON types → "any" (handled separately via isWKTJSON flag)
func coerceReturnType(field *protogen.Field) string {
	switch field.Desc.Kind() {
	case protoreflect.FloatKind:
		return "float64"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "int"
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return "any"
	default:
		return "any"
	}
}

// protoScalarGoType returns the concrete Go type that protoc-gen-go uses for a
// scalar proto kind (the type of the field on the pb oneof-wrapper struct).
// This is distinct from the GraphQL-aligned Go type: e.g. a uint32 field maps to
// the "Int" GraphQL scalar (Go int32) but its pb field is uint32. Oneof adapters
// must convert between the two. Returns "" for non-scalar kinds (message/enum/group).
func protoScalarGoType(k protoreflect.Kind) string {
	switch k {
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.StringKind:
		return "string"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int32"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"
	case protoreflect.FloatKind:
		return "float32"
	case protoreflect.DoubleKind:
		return "float64"
	case protoreflect.BytesKind:
		return "[]byte"
	default:
		return ""
	}
}

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
