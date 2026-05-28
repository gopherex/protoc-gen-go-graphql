package generator

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ── Oneof metadata ────────────────────────────────────────────────────────────

// oneofVariant describes a single field in a proto oneof.
type oneofVariant struct {
	// ProtoFieldName is the proto field name (snake_case), e.g. "not_found".
	ProtoFieldName string
	// GoFieldName is the Go exported field name, e.g. "NotFound".
	GoFieldName string
	// GQLTypeName is the GraphQL type for this variant's value:
	//   - for message variants: the message's GoName (e.g. "Book")
	//   - for scalar variants: the GraphQL scalar name (e.g. "String")
	GQLTypeName string
	// IsMessage is true when the variant holds a message value.
	IsMessage bool
	// MessageGoName is the Go name of the held message (only set when IsMessage).
	MessageGoName string
	// WrapperGoName is the pbgql wrapper type Go name (e.g. "SearchResponseResultBook").
	WrapperGoName string
	// WrapperPbField is the pb wrapper struct name (e.g. "SearchResponse_Book").
	WrapperPbField string
	// Msg is the protogen message for this variant (only set when IsMessage).
	// Used by schema.go to emit the correct field list for the union member type.
	Msg *protogen.Message
}

// oneofInfo describes a single proto oneof within a message.
type oneofInfo struct {
	// Msg is the containing proto message.
	Msg *protogen.Message
	// MsgGoName is the Go name of the containing message (e.g. "SearchResponse").
	MsgGoName string
	// OneofGoName is the exported Go name of the oneof (e.g. "Result").
	OneofGoName string
	// ProtoName is the proto oneof name (e.g. "result").
	ProtoName string
	// GQLFieldName is the camelCase GraphQL field name (e.g. "result").
	GQLFieldName string
	// UnionGQLName is the GraphQL union type name (e.g. "SearchResponseResult").
	UnionGQLName string
	// InterfaceGoName is the pbgql Go interface name (e.g. "SearchResponseResult").
	InterfaceGoName string
	// IsOutput is true when this oneof is in an output (roleOutput) message.
	IsOutput bool
	// IsInput is true when this oneof is in an input (roleInput, isRequest) message.
	IsInput bool
	// Variants lists the individual oneof fields.
	Variants []oneofVariant
	// InputGQLName is the @oneOf GraphQL input name (e.g. "SearchRequestQuery").
	InputGQLName string
	// InputGoName is the pbgql Go struct name for the @oneOf input (e.g. "SearchRequestQuery").
	InputGoName string
	// MsgInputGoName is the pbgql Go struct for the whole request (e.g. "SearchRequestInput").
	// Empty when the message is a pure output message.
	MsgInputGoName string
	// PbMsgGoName is used in ToPb conversions (same as MsgGoName for pb types).
	PbMsgGoName string
}

// collectOneofs returns all non-synthetic oneofs in file f, annotated with
// their role (output/input) based on msgInfo.
func collectOneofs(f *protogen.File, msgInfo map[string]*messageInfo) []oneofInfo {
	return collectOneofsGraph(graphFromFile(f), msgInfo)
}

func collectOneofsGraph(g *graph, msgInfo map[string]*messageInfo) []oneofInfo {
	var result []oneofInfo

	for _, msg := range g.Messages {
		name := msg.GoIdent.GoName
		mi := msgInfo[messageKey(msg)]
		if mi == nil {
			continue
		}

		for _, oo := range msg.Oneofs {
			// Skip synthetic oneofs (proto3 optional fields).
			if oo.Desc.IsSynthetic() {
				continue
			}

			protoName := string(oo.Desc.Name())
			ooGoName := oo.GoName            // exported, e.g. "Result"
			gqlField := fieldName(protoName) // camelCase, e.g. "result"
			unionName := name + ooGoName     // e.g. "SearchResponseResult"

			var variants []oneofVariant
			for _, field := range oo.Fields {
				fProtoName := string(field.Desc.Name())
				fGoName := string(field.GoName)
				wrapperName := unionName + fGoName // e.g. "SearchResponseResultBook"

				var gqlType, msgGoName string
				isMsg := false
				if field.Desc.Kind() == protoreflect.MessageKind {
					fqn := string(field.Desc.Message().FullName())
					if wktScalar, ok := wellKnownGQLType[fqn]; ok {
						// WKT message in a oneof: treat as a scalar variant (not a message wrapper).
						gqlType = wktScalar
					} else {
						isMsg = true
						msgGoName = field.Message.GoIdent.GoName
						gqlType = msgGoName
					}
				} else if field.Desc.Kind() == protoreflect.EnumKind {
					gqlType = string(field.Enum.GoIdent.GoName)
				} else {
					gqlType = scalarForKind(field.Desc.Kind())
					if gqlType == "" {
						gqlType = "String"
					}
				}

				// pb wrapper struct field name: <Msg>_<GoFieldName>, e.g. SearchResponse_Book
				pbWrapperField := name + "_" + fGoName

				var variantMsg *protogen.Message
				if isMsg {
					variantMsg = field.Message
				}
				variants = append(variants, oneofVariant{
					ProtoFieldName: fProtoName,
					GoFieldName:    fGoName,
					GQLTypeName:    gqlType,
					IsMessage:      isMsg,
					MessageGoName:  msgGoName,
					WrapperGoName:  wrapperName,
					WrapperPbField: pbWrapperField,
					Msg:            variantMsg,
				})
			}

			oi := oneofInfo{
				Msg:             msg,
				MsgGoName:       name,
				OneofGoName:     ooGoName,
				ProtoName:       protoName,
				GQLFieldName:    gqlField,
				UnionGQLName:    unionName,
				InterfaceGoName: unionName,
				Variants:        variants,
				PbMsgGoName:     name,
			}

			if mi.role.has(roleOutput) {
				oi.IsOutput = true
			}
			if mi.role.has(roleInput) && mi.isRequest {
				oi.IsInput = true
				oi.MsgInputGoName = name + "Input" // e.g. "SearchRequestInput"
			}
			// Input @oneOf names. GraphQL has one global namespace for object,
			// union, and input names, so an input oneof that is also an output
			// union must use a distinct name.
			oi.InputGQLName = name + ooGoName // e.g. "SearchRequestQuery"
			oi.InputGoName = name + ooGoName  // same as Go struct name in pbgql
			if oi.IsInput && oi.IsOutput {
				oi.InputGQLName += "Input"
				oi.InputGoName += "Input"
			}

			result = append(result, oi)
		}
	}

	return result
}

// ── pbgql oneof adapter emitter ───────────────────────────────────────────────

// buildOneofAdapter emits the pbgql/<msg_lower>_oneof.go source for a single
// message that contains one or more oneofs. It produces:
//   - For each output oneof: the union Go interface + per-variant wrapper structs
//   - a WrapXxx() conversion helper.
//   - For each input oneof: the @oneOf input struct + the request intermediate
//     struct + a ToPbXxx() conversion helper.
func buildOneofAdapter(ois []oneofInfo, pbImport string) string {
	if len(ois) == 0 {
		return ""
	}

	var sb strings.Builder

	fmt.Fprintf(&sb, "// Package pbgql provides GraphQL binding adapters for proto types.\n")
	fmt.Fprintf(&sb, "// Code generated by protoc-gen-go-graphql. DO NOT EDIT.\n")
	sb.WriteString("package pbgql\n")
	sb.WriteString("\n")
	sb.WriteString("import (\n")
	fmt.Fprintf(&sb, "\tpb %q\n", pbImport)
	sb.WriteString(")\n")
	sb.WriteString("\n")

	for _, oi := range ois {
		if oi.IsOutput {
			emitOutputOneofTypes(&sb, oi)
		}
		if oi.IsInput {
			emitInputOneofTypes(&sb, oi)
		}
	}

	return sb.String()
}

// emitOutputOneofTypes emits the union interface, per-variant wrappers, and Wrap helper.
func emitOutputOneofTypes(sb *strings.Builder, oi oneofInfo) {
	markerMethod := "Is" + oi.InterfaceGoName

	// Union interface.
	fmt.Fprintf(sb, "// %s is the Go interface backing the GraphQL union %q.\n",
		oi.InterfaceGoName, oi.UnionGQLName)
	fmt.Fprintf(sb, "// gqlgen requires a Go interface for union models.\n")
	fmt.Fprintf(sb, "type %s interface { %s() }\n", oi.InterfaceGoName, markerMethod)
	sb.WriteString("\n")

	// Per-variant wrappers.
	for _, v := range oi.Variants {
		fmt.Fprintf(sb, "// %s wraps *pb.%s to implement %s.\n",
			v.WrapperGoName, v.MessageGoName, oi.InterfaceGoName)
		if v.IsMessage {
			fmt.Fprintf(sb, "type %s struct{ *pb.%s }\n", v.WrapperGoName, v.MessageGoName)
		} else {
			// Scalar variant: just hold the value.
			gqlScalarToGo := map[string]string{
				"String": "string", "Boolean": "bool",
				"Int": "int32", "Int64": "int64", "Uint64": "uint64",
				"Float": "float64", "Bytes": "[]byte",
			}
			goType := gqlScalarToGo[v.GQLTypeName]
			if goType == "" {
				goType = "any"
			}
			fmt.Fprintf(sb, "type %s struct{ Value %s }\n", v.WrapperGoName, goType)
		}
		fmt.Fprintf(sb, "func (%s) %s() {}\n", v.WrapperGoName, markerMethod)
		sb.WriteString("\n")
	}

	// Wrap helper function.
	fmt.Fprintf(sb, "// Wrap%s converts the pb oneof interface value into the %s union wrapper.\n",
		oi.InterfaceGoName, oi.UnionGQLName)
	fmt.Fprintf(sb, "// Returns nil if obj is nil or has no oneof set.\n")
	fmt.Fprintf(sb, "func Wrap%s(obj *pb.%s) %s {\n", oi.InterfaceGoName, oi.MsgGoName, oi.InterfaceGoName)
	sb.WriteString("\tif obj == nil {\n\t\treturn nil\n\t}\n")
	fmt.Fprintf(sb, "\tswitch v := obj.Get%s().(type) {\n", oi.OneofGoName)
	for _, v := range oi.Variants {
		fmt.Fprintf(sb, "\tcase *pb.%s:\n", v.WrapperPbField)
		if v.IsMessage {
			fmt.Fprintf(sb, "\t\treturn %s{v.%s}\n", v.WrapperGoName, v.GoFieldName)
		} else {
			fmt.Fprintf(sb, "\t\treturn %s{Value: v.%s}\n", v.WrapperGoName, v.GoFieldName)
		}
	}
	sb.WriteString("\tdefault:\n\t\treturn nil\n\t}\n}\n\n")
}

// emitInputOneofTypes emits the @oneOf input struct, the request intermediate struct,
// and the ToPb conversion helper.
func emitInputOneofTypes(sb *strings.Builder, oi oneofInfo) {
	// @oneOf input struct (flat nullable fields).
	fmt.Fprintf(sb, "// %s is the Go model for the GraphQL @oneOf input %q.\n",
		oi.InputGoName, oi.InputGQLName)
	fmt.Fprintf(sb, "// gqlgen populates exactly one field (the rest are nil).\n")
	fmt.Fprintf(sb, "type %s struct {\n", oi.InputGoName)
	for _, v := range oi.Variants {
		goType := oneofVariantGoType(v)
		fmt.Fprintf(sb, "\t%s *%s `json:%q`\n", v.GoFieldName, goType, v.ProtoFieldName)
	}
	sb.WriteString("}\n\n")

	// Request intermediate struct.
	fmt.Fprintf(sb, "// %s is the intermediate Go model for the %s input.\n",
		oi.MsgInputGoName, oi.MsgGoName)
	fmt.Fprintf(sb, "// It wraps the @oneOf input so gqlgen can populate it; the\n")
	fmt.Fprintf(sb, "// resolver converts it to *pb.%s via ToPb%s.\n",
		oi.MsgGoName, oi.MsgGoName)
	fmt.Fprintf(sb, "type %s struct {\n", oi.MsgInputGoName)
	gqlFieldName := oi.GQLFieldName
	capField := strings.ToUpper(gqlFieldName[:1]) + gqlFieldName[1:]
	fmt.Fprintf(sb, "\t%s *%s `json:%q`\n", capField, oi.InputGoName, oi.ProtoName)
	sb.WriteString("}\n\n")

	// ToPb conversion function.
	fmt.Fprintf(sb, "// ToPb%s converts a %s to a *pb.%s by mapping the @oneOf field.\n",
		oi.MsgGoName, oi.MsgInputGoName, oi.MsgGoName)
	fmt.Fprintf(sb, "func ToPb%s(r *%s) *pb.%s {\n", oi.MsgGoName, oi.MsgInputGoName, oi.MsgGoName)
	sb.WriteString("\tif r == nil {\n")
	fmt.Fprintf(sb, "\t\treturn &pb.%s{}\n", oi.MsgGoName)
	sb.WriteString("\t}\n")
	fmt.Fprintf(sb, "\treq := &pb.%s{}\n", oi.MsgGoName)
	fmt.Fprintf(sb, "\tif r.%s != nil {\n", capField)
	sb.WriteString("\t\tswitch {\n")
	for _, v := range oi.Variants {
		fmt.Fprintf(sb, "\t\tcase r.%s.%s != nil:\n", capField, v.GoFieldName)
		if v.IsMessage {
			fmt.Fprintf(sb, "\t\t\treq.%s = &pb.%s{%s: r.%s.%s}\n",
				oi.OneofGoName, v.WrapperPbField, v.GoFieldName, capField, v.GoFieldName)
		} else {
			fmt.Fprintf(sb, "\t\t\treq.%s = &pb.%s{%s: *r.%s.%s}\n",
				oi.OneofGoName, v.WrapperPbField, v.GoFieldName, capField, v.GoFieldName)
		}
	}
	sb.WriteString("\t\t}\n\t}\n\treturn req\n}\n\n")
}

// oneofVariantGoType returns the Go type for a oneof variant's value in the @oneOf input struct.
func oneofVariantGoType(v oneofVariant) string {
	if v.IsMessage {
		// Message variants hold the message type (pointer unwrapped in the struct, pointer added in field tag).
		return "pb." + v.MessageGoName
	}
	switch v.GQLTypeName {
	case "String":
		return "string"
	case "Boolean":
		return "bool"
	case "Int":
		return "int32"
	case "Int64":
		return "int64"
	case "Uint64":
		return "uint64"
	case "Float":
		return "float64"
	case "Bytes":
		return "[]byte"
	default:
		return "any"
	}
}
