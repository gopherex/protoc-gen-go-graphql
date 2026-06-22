package generator

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gopherex/protoc-gen-go-graphql/graphqlopt"
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
	// PbScalarGoType is the concrete Go type protoc-gen-go uses for a scalar
	// variant's pb field (e.g. "uint32"). Empty for message/enum/WKT variants.
	// Used to convert between the pb field type and the GraphQL-aligned Go type
	// in the Wrap/ToPb helpers (e.g. uint32 ⇄ int32 for the "Int" scalar).
	PbScalarGoType string
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
	// InputMode is the OneofOptions.input_mode (UNSPECIFIED/DIRECTIVE → schema
	// @oneOf enforcement; ALL_NULLABLE → plain input + runtime exactly-one check).
	InputMode graphqlopt.OneofInputMode
}

// isAllNullable reports whether the oneof input uses the ALL_NULLABLE mode
// (plain input object, runtime exactly-one enforcement). UNSPECIFIED and
// ONEOF_DIRECTIVE both map to the schema-enforced @oneOf directive.
func (oi oneofInfo) isAllNullable() bool {
	return oi.InputMode == graphqlopt.OneofInputMode_ALL_NULLABLE
}

// collectOneofsGraph returns all non-synthetic oneofs in the graph, annotated
// with their role (output/input) based on msgInfo.
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
			// Go-derived base used for wrapper type names, the pbgql interface,
			// and pb wrapper struct fields — these stay keyed by Go names.
			unionName := name + ooGoName // e.g. "SearchResponseResult"
			// The GraphQL union name honors an OneofOptions.union_name override;
			// only the schema union name + its gqlgen.yml binding key change.
			unionGQLName := unionName
			inputMode := graphqlopt.OneofInputMode_ONEOF_INPUT_UNSPECIFIED
			if o := oneofOpts(oo); o != nil {
				if o.GetUnionName() != "" {
					unionGQLName = o.GetUnionName()
				}
				inputMode = o.GetInputMode()
			}

			var variants []oneofVariant
			for _, field := range oo.Fields {
				fProtoName := string(field.Desc.Name())
				fGoName := string(field.GoName)
				wrapperName := unionName + fGoName // e.g. "SearchResponseResultBook"

				var gqlType, msgGoName, pbScalarGoType string
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
					pbScalarGoType = protoScalarGoType(field.Desc.Kind())
				}

				// pb oneof-wrapper struct Go name. protoc-gen-go normally names it
				// <Msg>_<GoFieldName> (e.g. SearchResponse_Book), but appends a
				// disambiguating underscore when that name collides with another
				// generated type — e.g. a nested message Database_External forces the
				// oneof wrapper to be Database_External_. Use the actual Go ident from
				// protogen rather than re-deriving it, so collisions are handled.
				pbWrapperField := field.GoIdent.GoName

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
					PbScalarGoType: pbScalarGoType,
				})
			}

			oi := oneofInfo{
				Msg:             msg,
				MsgGoName:       name,
				OneofGoName:     ooGoName,
				ProtoName:       protoName,
				GQLFieldName:    gqlField,
				UnionGQLName:    unionGQLName,
				InterfaceGoName: unionName,
				Variants:        variants,
				PbMsgGoName:     name,
				InputMode:       inputMode,
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
			// union must use a distinct name. The GraphQL-side name honors the
			// union_name override; the pbgql Go struct name stays Go-derived.
			oi.InputGQLName = unionGQLName   // e.g. "SearchRequestQuery"
			oi.InputGoName = name + ooGoName // Go struct name in pbgql
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

	// Import management mirrors resolvers.go: the oneof message's own package is
	// aliased "pb"; variant payload messages may live in OTHER proto packages and
	// get pb<N> aliases. Without this, a oneof whose variant payload is an imported
	// message (e.g. a deployment message with a variant of type common.Cmd) would
	// be mis-qualified as pb.Cmd and fail to compile.
	pbAliases := map[string]string{pbImport: "pb"}
	var extraImports []string
	addImport := func(p protogen.GoImportPath) {
		path := string(p)
		if path == "" || path == pbImport {
			return
		}
		if _, ok := pbAliases[path]; ok {
			return
		}
		pbAliases[path] = fmt.Sprintf("pb%d", len(pbAliases))
		extraImports = append(extraImports, path)
	}
	qual := func(id protogen.GoIdent) string {
		alias := pbAliases[string(id.GoImportPath)]
		if alias == "" {
			alias = "pb"
		}
		return alias + "." + id.GoName
	}
	for _, oi := range ois {
		for _, v := range oi.Variants {
			if v.IsMessage && v.Msg != nil {
				addImport(v.Msg.GoIdent.GoImportPath)
			}
		}
	}

	// Emit the type bodies first so the import block can drop unreferenced payload
	// aliases (an imported-and-unused package is a compile error).
	var body strings.Builder
	for _, oi := range ois {
		if oi.IsOutput {
			emitOutputOneofTypes(&body, oi, qual)
		}
		if oi.IsInput {
			emitInputOneofTypes(&body, oi, qual)
		}
	}
	bodyStr := body.String()

	// A message whose oneofs produce no output union and no input @oneOf (a oneof
	// in a nested input message, or a message of a skipped service) yields an empty
	// body. Emit no file at all rather than a file with a lone dangling pb import.
	if strings.TrimSpace(bodyStr) == "" {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "// Package pbgql provides GraphQL binding adapters for proto types.\n")
	fmt.Fprintf(&sb, "// Code generated by protoc-gen-go-graphql. DO NOT EDIT.\n")
	sb.WriteString("package pbgql\n")
	sb.WriteString("\n")
	// fmt is needed only when an ALL_NULLABLE input oneof is present: its ToPb
	// shim calls fmt.Errorf for the runtime exactly-one check. DIRECTIVE-mode
	// shims also return an error now, but always a nil one (no fmt usage).
	needsFmt := false
	for _, oi := range ois {
		if oi.IsInput && oi.isAllNullable() {
			needsFmt = true
			break
		}
	}
	sb.WriteString("import (\n")
	if needsFmt {
		sb.WriteString("\t\"fmt\"\n")
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "\tpb %q\n", pbImport)
	for _, path := range extraImports {
		alias := pbAliases[path]
		if !strings.Contains(bodyStr, alias+".") {
			continue // payload package not referenced by any emitted type
		}
		fmt.Fprintf(&sb, "\t%s %q\n", alias, path)
	}
	sb.WriteString(")\n")
	sb.WriteString("\n")
	sb.WriteString(bodyStr)

	return sb.String()
}

// gqlScalarGoType maps a GraphQL scalar name to the Go type used for a scalar
// oneof variant's value (the type gqlgen binds). Returns "any" for unmapped names
// (e.g. enum or WKT-scalar variants, which bind through the empty interface).
func gqlScalarGoType(gql string) string {
	switch gql {
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

// emitOutputOneofTypes emits the union interface, per-variant wrappers, and Wrap helper.
func emitOutputOneofTypes(sb *strings.Builder, oi oneofInfo, qual func(protogen.GoIdent) string) {
	markerMethod := "Is" + oi.InterfaceGoName

	// Union interface.
	fmt.Fprintf(sb, "// %s is the Go interface backing the GraphQL union %q.\n",
		oi.InterfaceGoName, oi.UnionGQLName)
	fmt.Fprintf(sb, "// gqlgen requires a Go interface for union models.\n")
	fmt.Fprintf(sb, "type %s interface { %s() }\n", oi.InterfaceGoName, markerMethod)
	sb.WriteString("\n")

	// Per-variant wrappers.
	for _, v := range oi.Variants {
		if v.IsMessage {
			payload := qual(v.Msg.GoIdent)
			fmt.Fprintf(sb, "// %s wraps *%s to implement %s.\n",
				v.WrapperGoName, payload, oi.InterfaceGoName)
			fmt.Fprintf(sb, "type %s struct{ *%s }\n", v.WrapperGoName, payload)
		} else {
			// Scalar variant: hold the GraphQL-aligned value.
			fmt.Fprintf(sb, "// %s wraps the %q scalar variant to implement %s.\n",
				v.WrapperGoName, v.GQLTypeName, oi.InterfaceGoName)
			fmt.Fprintf(sb, "type %s struct{ Value %s }\n", v.WrapperGoName, gqlScalarGoType(v.GQLTypeName))
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
			// Convert the pb field to the GraphQL-aligned Go type (e.g. uint32→int32,
			// float32→float64). The conversion is an identity no-op for matching types.
			val := "v." + v.GoFieldName
			if gt := gqlScalarGoType(v.GQLTypeName); gt != "any" && v.PbScalarGoType != "" {
				val = gt + "(" + val + ")"
			}
			fmt.Fprintf(sb, "\t\treturn %s{Value: %s}\n", v.WrapperGoName, val)
		}
	}
	sb.WriteString("\tdefault:\n\t\treturn nil\n\t}\n}\n\n")
}

// emitInputOneofTypes emits the @oneOf input struct, the request intermediate struct,
// and the ToPb conversion helper.
func emitInputOneofTypes(sb *strings.Builder, oi oneofInfo, qual func(protogen.GoIdent) string) {
	// Input struct (flat nullable fields). In DIRECTIVE mode the schema carries
	// @oneOf so gqlgen guarantees ≤1 field is set; in ALL_NULLABLE mode the
	// "exactly one" rule is enforced at runtime by ToPb below.
	if oi.isAllNullable() {
		fmt.Fprintf(sb, "// %s is the Go model for the GraphQL input %q (ALL_NULLABLE mode).\n",
			oi.InputGoName, oi.InputGQLName)
		fmt.Fprintf(sb, "// All fields are nullable; ToPb%s enforces that exactly one is set.\n", oi.MsgGoName)
	} else {
		fmt.Fprintf(sb, "// %s is the Go model for the GraphQL @oneOf input %q.\n",
			oi.InputGoName, oi.InputGQLName)
		fmt.Fprintf(sb, "// gqlgen populates exactly one field (the rest are nil).\n")
	}
	fmt.Fprintf(sb, "type %s struct {\n", oi.InputGoName)
	for _, v := range oi.Variants {
		goType := oneofVariantGoType(v, qual)
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

	// ToPb conversion function. The signature uniformly returns an error across
	// both modes; DIRECTIVE always returns a nil error (gqlgen guarantees ≤1
	// variant), ALL_NULLABLE returns an error unless exactly one variant is set.
	if oi.isAllNullable() {
		fmt.Fprintf(sb, "// ToPb%s converts a %s to a *pb.%s, enforcing that exactly\n",
			oi.MsgGoName, oi.MsgInputGoName, oi.MsgGoName)
		fmt.Fprintf(sb, "// one of the input's nullable variant fields is set (ALL_NULLABLE mode).\n")
	} else {
		fmt.Fprintf(sb, "// ToPb%s converts a %s to a *pb.%s by mapping the @oneOf field.\n",
			oi.MsgGoName, oi.MsgInputGoName, oi.MsgGoName)
	}
	fmt.Fprintf(sb, "func ToPb%s(r *%s) (*pb.%s, error) {\n", oi.MsgGoName, oi.MsgInputGoName, oi.MsgGoName)
	sb.WriteString("\tif r == nil {\n")
	fmt.Fprintf(sb, "\t\treturn &pb.%s{}, nil\n", oi.MsgGoName)
	sb.WriteString("\t}\n")
	fmt.Fprintf(sb, "\treq := &pb.%s{}\n", oi.MsgGoName)

	if oi.isAllNullable() {
		// Build a human-readable list of the GraphQL field names for the error.
		gqlFields := make([]string, len(oi.Variants))
		for i, v := range oi.Variants {
			gqlFields[i] = fieldName(v.ProtoFieldName)
		}
		fieldList := strings.Join(gqlFields, ", ")
		// Count set variants; require exactly one.
		fmt.Fprintf(sb, "\tif r.%s == nil {\n", capField)
		fmt.Fprintf(sb, "\t\treturn nil, fmt.Errorf(\"exactly one of {%s} must be set for %s\")\n", fieldList, oi.InputGQLName)
		sb.WriteString("\t}\n")
		sb.WriteString("\tset := 0\n")
		for _, v := range oi.Variants {
			fmt.Fprintf(sb, "\tif r.%s.%s != nil {\n\t\tset++\n\t}\n", capField, v.GoFieldName)
		}
		sb.WriteString("\tif set != 1 {\n")
		fmt.Fprintf(sb, "\t\treturn nil, fmt.Errorf(\"exactly one of {%s} must be set for %s\")\n", fieldList, oi.InputGQLName)
		sb.WriteString("\t}\n")
		sb.WriteString("\tswitch {\n")
		for _, v := range oi.Variants {
			fmt.Fprintf(sb, "\tcase r.%s.%s != nil:\n", capField, v.GoFieldName)
			if v.IsMessage {
				fmt.Fprintf(sb, "\t\treq.%s = &pb.%s{%s: r.%s.%s}\n",
					oi.OneofGoName, v.WrapperPbField, v.GoFieldName, capField, v.GoFieldName)
			} else {
				fmt.Fprintf(sb, "\t\treq.%s = &pb.%s{%s: %s}\n",
					oi.OneofGoName, v.WrapperPbField, v.GoFieldName, toPbScalarExpr(v, capField))
			}
		}
		sb.WriteString("\t}\n\treturn req, nil\n}\n\n")
		return
	}

	// DIRECTIVE mode: schema @oneOf guarantees ≤1 variant; pick the set one.
	fmt.Fprintf(sb, "\tif r.%s != nil {\n", capField)
	sb.WriteString("\t\tswitch {\n")
	for _, v := range oi.Variants {
		fmt.Fprintf(sb, "\t\tcase r.%s.%s != nil:\n", capField, v.GoFieldName)
		if v.IsMessage {
			fmt.Fprintf(sb, "\t\t\treq.%s = &pb.%s{%s: r.%s.%s}\n",
				oi.OneofGoName, v.WrapperPbField, v.GoFieldName, capField, v.GoFieldName)
		} else {
			fmt.Fprintf(sb, "\t\t\treq.%s = &pb.%s{%s: %s}\n",
				oi.OneofGoName, v.WrapperPbField, v.GoFieldName, toPbScalarExpr(v, capField))
		}
	}
	sb.WriteString("\t\t}\n\t}\n\treturn req, nil\n}\n\n")
}

// oneofVariantGoType returns the Go type for a oneof variant's value in the @oneOf
// input struct. Message variants are qualified by their own proto package (which
// may differ from the oneof message's package).
func oneofVariantGoType(v oneofVariant, qual func(protogen.GoIdent) string) string {
	if v.IsMessage {
		// Message variants hold the message type (pointer unwrapped in the struct, pointer added in field tag).
		return qual(v.Msg.GoIdent)
	}
	return gqlScalarGoType(v.GQLTypeName)
}

// toPbScalarExpr renders the value expression that assigns a scalar @oneOf input
// field back to its pb oneof-wrapper field, converting from the GraphQL-aligned Go
// type to the concrete pb scalar type (e.g. int32→uint32). Identity for matching
// types; no conversion when the pb scalar type is unknown (enum/WKT variants).
func toPbScalarExpr(v oneofVariant, capField string) string {
	src := fmt.Sprintf("*r.%s.%s", capField, v.GoFieldName)
	if v.PbScalarGoType != "" {
		return v.PbScalarGoType + "(" + src + ")"
	}
	return src
}
