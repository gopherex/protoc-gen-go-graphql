package generator

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
	descriptorpb "google.golang.org/protobuf/types/descriptorpb"

	"github.com/gopherex/protoc-gen-go-graphql/graphqlopt"
)

// isEmptyMessage returns true if msg is a non-map-entry proto message with zero fields.
// Such messages are "empty" and require special handling in schema/resolver generation.
func isEmptyMessage(msg *protogen.Message) bool {
	if msg == nil || msg.Desc.IsMapEntry() {
		return false
	}
	return len(msg.Fields) == 0
}

// wellKnownGQLType maps a fully qualified WKT message name to its GraphQL scalar name.
var wellKnownGQLType = map[string]string{
	"google.protobuf.Timestamp": "Timestamp",
	"google.protobuf.Duration":  "Duration",
	"google.protobuf.Struct":    "JSON",
	"google.protobuf.Value":     "JSON",
	"google.protobuf.ListValue": "JSON",
	"google.protobuf.Any":       "JSON",
	"google.protobuf.Empty":     "JSON",
	"google.protobuf.FieldMask": "FieldMask",
	// Wrapper types → per-wrapper nullable scalar (protojson: bare inner value or null).
	"google.protobuf.DoubleValue": "DoubleValue",
	"google.protobuf.FloatValue":  "FloatValue",
	"google.protobuf.Int32Value":  "Int32Value",
	"google.protobuf.UInt32Value": "UInt32Value",
	"google.protobuf.Int64Value":  "Int64Value",
	"google.protobuf.UInt64Value": "UInt64Value",
	"google.protobuf.BoolValue":   "BoolValue",
	"google.protobuf.StringValue": "StringValue",
	"google.protobuf.BytesValue":  "BytesValue",
}

// messageRole describes how a message is used.
type messageRole int

const (
	roleOutput messageRole = 1 << 0 // used in output context (RPC response reachable)
	roleInput  messageRole = 1 << 1 // used in input context (RPC request reachable)
)

func (r messageRole) has(other messageRole) bool {
	return r&other != 0
}

// messageInfo groups derived info for a message.
type messageInfo struct {
	role      messageRole
	isRequest bool // top-level RPC request (keeps original name as input)
}

func inputTypeName(msg *protogen.Message, msgInfo map[string]*messageInfo) string {
	name := msg.GoIdent.GoName
	mi := msgInfo[messageKey(msg)]
	if mi != nil && mi.isRequest && mi.role.has(roleOutput) {
		return name + "Input"
	}
	return name
}

// analyzeMessages returns a map from messageKey to messageInfo for all non-map-entry
// messages in f. It determines which messages are used as output types, input
// types, or both.
func analyzeMessages(f *protogen.File) map[string]*messageInfo {
	return analyzeMessagesGraph(graphFromFile(f))
}

func analyzeMessagesGraph(g *graph) map[string]*messageInfo {
	info := map[string]*messageInfo{}

	// Initialize all top-level messages.
	var initAll func(msgs []*protogen.Message)
	initAll = func(msgs []*protogen.Message) {
		for _, msg := range msgs {
			if msg.Desc.IsMapEntry() {
				continue
			}
			info[messageKey(msg)] = &messageInfo{}
			initAll(msg.Messages)
		}
	}
	for _, msg := range g.Messages {
		info[messageKey(msg)] = &messageInfo{}
	}

	// Mark top-level request messages.
	for _, svc := range g.Services {
		for _, m := range includedMethods(svc) {
			if mi, ok := info[messageKey(m.Input)]; ok {
				mi.isRequest = true
				mi.role |= roleInput
			}
		}
	}

	// BFS from RPC responses → mark output.
	var markOutput func(msg *protogen.Message)
	markOutput = func(msg *protogen.Message) {
		mi, ok := info[messageKey(msg)]
		if !ok {
			return
		}
		if mi.role&roleOutput != 0 {
			return // already visited
		}
		mi.role |= roleOutput
		for _, field := range msg.Fields {
			if field.Desc.IsMap() {
				continue
			}
			if field.Desc.Kind() == protoreflect.MessageKind {
				fqn := string(field.Desc.Message().FullName())
				if _, wkt := wellKnownGQLType[fqn]; !wkt {
					markOutput(field.Message)
				}
			}
		}
	}

	// BFS from RPC requests → mark input (for nested messages).
	var markInput func(msg *protogen.Message)
	markInput = func(msg *protogen.Message) {
		for _, field := range msg.Fields {
			if field.Desc.IsMap() {
				continue
			}
			if field.Desc.Kind() == protoreflect.MessageKind {
				fqn := string(field.Desc.Message().FullName())
				if _, wkt := wellKnownGQLType[fqn]; wkt {
					continue
				}
				mi, ok := info[messageKey(field.Message)]
				if !ok {
					continue
				}
				if mi.role&roleInput != 0 {
					continue // already visited
				}
				mi.role |= roleInput
				markInput(field.Message)
			}
		}
	}

	for _, svc := range g.Services {
		for _, m := range includedMethods(svc) {
			markOutput(m.Output)
			markInput(m.Input)
		}
	}

	return info
}

// allMessages returns a flat slice of all non-map-entry messages in the file,
// including nested messages, in DFS order (parent before children).
func allMessages(f *protogen.File) []*protogen.Message {
	return graphFromFile(f).Messages
}

// allEnums returns all enums in the file, including those nested inside messages.
func allEnums(f *protogen.File) []*protogen.Enum {
	return graphFromFile(f).Enums
}

// buildSchema walks f's descriptors and returns a GraphQL SDL string.
// The output matches spike/schema.graphql for the golden proto:
//  1. @goField and @oneOf directive declarations
//  2. Scalar declarations (used only)
//  3. Enum types
//  4. Union types (output oneofs)
//  5. Domain output types (roleOutput & roleInput) — e.g. Author, Book
//  6. Input types (request messages and nested input types)
//  7. Pure output types (roleOutput & !roleInput) — e.g. GetBookResponse
//  8. Operation roots (Query / Mutation / Subscription)
func buildSchema(f *protogen.File) (string, error) {
	return buildSchemaGraph(graphFromFile(f))
}

func buildSchemaGraph(g *graph) (string, error) {
	var sb strings.Builder

	msgInfo := analyzeMessagesGraph(g)
	ois := collectOneofsGraph(g, msgInfo)

	// Index oneofs by message name for fast lookup.
	oneofsByMsg := map[string][]oneofInfo{}
	for _, oi := range ois {
		key := messageKey(oi.Msg)
		oneofsByMsg[key] = append(oneofsByMsg[key], oi)
	}

	// 1. Directive declarations (must be explicit per spike-findings §5).
	sb.WriteString("directive @goField(forceResolver: Boolean, name: String) on FIELD_DEFINITION | INPUT_FIELD_DEFINITION\n")
	// @oneOf is needed when any message has an input oneof.
	hasInputOneof := false
	for _, oi := range ois {
		if oi.IsInput {
			hasInputOneof = true
			break
		}
	}
	if hasInputOneof {
		sb.WriteString("directive @oneOf on INPUT_OBJECT\n")
	}
	// @idempotent is needed when at least one IDEMPOTENT mutation exists.
	if hasAnyIdempotentMutationGraph(g) {
		sb.WriteString("directive @idempotent on FIELD_DEFINITION\n")
	}
	sb.WriteString("\n")

	// 2. Scalar declarations (only those actually used).
	usedScalars := collectUsedScalarsGraph(g)
	for _, sc := range []string{
		"Int64", "Uint64", "Bytes", "Timestamp", "Duration", "JSON", "FieldMask",
		"DoubleValue", "FloatValue", "Int32Value", "UInt32Value",
		"Int64Value", "UInt64Value", "BoolValue", "StringValue", "BytesValue",
	} {
		if usedScalars[sc] {
			fmt.Fprintf(&sb, "scalar %s\n", sc)
		}
	}
	if len(usedScalars) > 0 {
		sb.WriteString("\n")
	}

	// 3. Enum types (including nested enums).
	for _, e := range g.Enums {
		emitEnum(&sb, e) // emitEnum already adds trailing blank line
	}

	// 4. Union types (output oneofs): emitted before the types that use them.
	hasUnions := false
	for _, oi := range ois {
		if oi.IsOutput {
			hasUnions = true
			// Emit member types for the union (wrapper types that will bind to pbgql wrappers).
			for _, v := range oi.Variants {
				emitUnionMemberType(&sb, v)
			}
			// Emit the union declaration.
			memberNames := make([]string, len(oi.Variants))
			for i, v := range oi.Variants {
				memberNames[i] = v.WrapperGoName
			}
			fmt.Fprintf(&sb, "union %s = %s\n", oi.UnionGQLName, strings.Join(memberNames, " | "))
		}
	}
	if hasUnions {
		sb.WriteString("\n")
	}

	// 5. Domain output types: roleOutput AND roleInput (used in both contexts).
	// E.g., Author and Book — used as output AND as templates for BookInput/AuthorInput.
	// Each may be single-line (Author) or multi-line (Book); add blank line after each.
	for _, msg := range g.Messages {
		mi := msgInfo[messageKey(msg)]
		if mi == nil {
			continue
		}
		if mi.role.has(roleOutput) && mi.role.has(roleInput) && (!mi.isRequest || inputTypeName(msg, msgInfo) != msg.GoIdent.GoName) {
			emitOutputType(&sb, msg, oneofsByMsg)
			sb.WriteString("\n") // blank line after each domain type
		}
	}

	// 6. Input types: in service/method order (request messages) with nested types.
	if err := emitInputTypesGraph(&sb, g, msgInfo, oneofsByMsg); err != nil {
		return "", err
	}
	sb.WriteString("\n") // blank line after input section

	// 7. Pure output types: roleOutput only (response wrappers like GetBookResponse).
	// Multiple single-line types appear consecutively without inter-item blank lines.
	for _, msg := range g.Messages {
		mi := msgInfo[messageKey(msg)]
		if mi == nil {
			continue
		}
		if mi.role.has(roleOutput) && !mi.role.has(roleInput) {
			emitOutputType(&sb, msg, oneofsByMsg)
		}
	}
	sb.WriteString("\n") // blank line after response types section

	// 8. Operation roots.
	emitOperationRootsGraph(&sb, g)

	return sb.String(), nil
}

// collectUsedScalars scans every message field and returns the set of custom
// GraphQL scalar names (Int64, Uint64, Bytes, Timestamp, Duration, JSON, etc.) used.
func collectUsedScalars(f *protogen.File) map[string]bool {
	return collectUsedScalarsGraph(graphFromFile(f))
}

func collectUsedScalarsGraph(g *graph) map[string]bool {
	used := map[string]bool{}
	for _, msg := range g.Messages {
		for _, field := range msg.Fields {
			sc := fieldGQLScalar(field)
			if sc != "" {
				switch sc {
				case "Int64", "Uint64", "Bytes", "Timestamp", "Duration", "JSON", "FieldMask",
					"DoubleValue", "FloatValue", "Int32Value", "UInt32Value",
					"Int64Value", "UInt64Value", "BoolValue", "StringValue", "BytesValue":
					used[sc] = true
				}
			}
		}
	}
	return used
}

// fieldGQLScalar returns the GraphQL scalar name for a field if it maps to a
// scalar (including JSON for maps and WKTs), or "" otherwise.
func fieldGQLScalar(field *protogen.Field) string {
	// Map fields → JSON scalar.
	if field.Desc.IsMap() {
		return "JSON"
	}
	switch field.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		fqn := string(field.Desc.Message().FullName())
		if sc, ok := wellKnownGQLType[fqn]; ok {
			return sc
		}
		return ""
	case protoreflect.EnumKind:
		return ""
	default:
		return scalarForKind(field.Desc.Kind())
	}
}

// fieldGQLType returns the full GraphQL type string for an output field,
// including nullability and list syntax.
func fieldGQLType(field *protogen.Field) string {
	// Map fields → JSON (nullable, no list wrapper — the field resolver returns any).
	if field.Desc.IsMap() {
		return "JSON"
	}

	base := fieldGQLBaseType(field)
	if base == "" {
		return "String" // fallback — should not happen
	}

	if field.Desc.IsList() {
		switch field.Desc.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			fqn := string(field.Desc.Message().FullName())
			if _, wkt := wellKnownGQLType[fqn]; wkt {
				return fmt.Sprintf("[%s!]!", base)
			}
			return fmt.Sprintf("[%s]!", base) // nullable inside
		default:
			return fmt.Sprintf("[%s!]!", base)
		}
	}

	// Singular field.
	switch field.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		// Message fields are nullable (proto3 semantics: message presence is optional).
		return base // no !
	default:
		// Scalars and enums: required in proto3.
		if field.Desc.HasOptionalKeyword() {
			return base // nullable
		}
		return base + "!"
	}
}

// fieldGQLBaseType returns the un-decorated GraphQL type name for a field.
func fieldGQLBaseType(field *protogen.Field) string {
	switch field.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		fqn := string(field.Desc.Message().FullName())
		if sc, ok := wellKnownGQLType[fqn]; ok {
			return sc
		}
		return string(field.Message.GoIdent.GoName)
	case protoreflect.EnumKind:
		return string(field.Enum.GoIdent.GoName)
	default:
		return scalarForKind(field.Desc.Kind())
	}
}

// outputFields returns all renderable output fields for a message (nil for map-entry msgs).
// oneofsByMsg maps messageKey to its oneof infos so oneof fields can be replaced
// by their union field with @goField(forceResolver: true).
func outputFields(msg *protogen.Message, oneofsByMsg map[string][]oneofInfo) []string {
	// Build a set of field names that belong to any non-synthetic oneof in this message.
	oneofFieldNames := map[string]bool{}
	// And build a map from oneof proto name → oneofInfo for union field emission.
	oneofByProtoName := map[string]oneofInfo{}
	for _, oi := range oneofsByMsg[messageKey(msg)] {
		if oi.IsOutput {
			for _, v := range oi.Variants {
				oneofFieldNames[v.ProtoFieldName] = true
			}
			oneofByProtoName[oi.ProtoName] = oi
		}
	}

	var lines []string
	emittedOneofs := map[string]bool{}

	for _, field := range msg.Fields {
		protoName := string(field.Desc.Name())

		// Check if this field is part of an output oneof.
		if oneofFieldNames[protoName] {
			// Find which oneof this field belongs to.
			if field.Oneof != nil && !field.Oneof.Desc.IsSynthetic() {
				ooProtoName := string(field.Oneof.Desc.Name())
				if oi, ok := oneofByProtoName[ooProtoName]; ok && !emittedOneofs[ooProtoName] {
					emittedOneofs[ooProtoName] = true
					// Emit a single union field for the whole oneof (with force-resolver).
					lines = append(lines, fmt.Sprintf("%s: %s @goField(forceResolver: true)",
						oi.GQLFieldName, oi.UnionGQLName))
				}
			}
			continue
		}

		var line string
		if field.Desc.IsMap() {
			gqlType := "JSON"
			goFieldName := fieldName(protoName)
			line = fmt.Sprintf("%s: %s @goField(forceResolver: true)", goFieldName, gqlType)
		} else if needsForceResolver(field) {
			goFieldName := fieldName(protoName)
			gqlType := fieldGQLType(field)
			line = fmt.Sprintf("%s: %s @goField(forceResolver: true)", goFieldName, gqlType)
		} else {
			goFieldName := fieldName(protoName)
			gqlType := fieldGQLType(field)
			line = fmt.Sprintf("%s: %s", goFieldName, gqlType)
		}
		lines = append(lines, line)
	}
	return lines
}

// emitEnum emits an enum type declaration (with trailing blank line).
func emitEnum(sb *strings.Builder, e *protogen.Enum) {
	fmt.Fprintf(sb, "enum %s {", e.GoIdent.GoName)
	for _, v := range e.Values {
		fmt.Fprintf(sb, " %s", v.Desc.Name())
	}
	sb.WriteString(" }\n\n")
}

// emitOutputType emits a `type` declaration (NO trailing blank line — callers add section separators).
// Single-field types use inline format; multi-field use multi-line.
// Empty messages (no fields) emit a placeholder: ok: Boolean! @goField(forceResolver: true)
func emitOutputType(sb *strings.Builder, msg *protogen.Message, oneofsByMsg map[string][]oneofInfo) {
	if msg.Desc.IsMapEntry() {
		return
	}
	fields := outputFields(msg, oneofsByMsg)
	if len(fields) == 0 {
		// Empty message: emit a placeholder field to satisfy GraphQL (no blank field names).
		fmt.Fprintf(sb, "type %s { ok: Boolean! @goField(forceResolver: true) }\n", msg.GoIdent.GoName)
		return
	}
	if len(fields) == 1 {
		// Inline format.
		fmt.Fprintf(sb, "type %s { %s }\n", msg.GoIdent.GoName, fields[0])
	} else {
		// Multi-line format.
		fmt.Fprintf(sb, "type %s {\n", msg.GoIdent.GoName)
		for _, f := range fields {
			sb.WriteString("  ")
			sb.WriteString(f)
			sb.WriteString("\n")
		}
		sb.WriteString("}\n")
	}
}

// emitUnionMemberType emits a `type` declaration for a single union member wrapper.
// For message variants, the wrapper has the same fields as the underlying pb message
// (the Go struct embeds *pb.Msg, so gqlgen resolves all fields via embedding).
// For scalar variants, the wrapper has a single `value` field.
func emitUnionMemberType(sb *strings.Builder, v oneofVariant) {
	if !v.IsMessage {
		// Scalar variant: a simple type with a single value field.
		fmt.Fprintf(sb, "type %s { value: %s! }\n", v.WrapperGoName, v.GQLTypeName)
		return
	}
	// Message variant: emit the same fields as the underlying message.
	// The wrapper Go struct embeds *pb.Msg, so all fields resolve via embedding.
	// We pass an empty oneofsByMsg here because the underlying message's fields
	// don't themselves have union substitution needed at this emission point.
	fields := outputFields(v.Msg, map[string][]oneofInfo{})
	if len(fields) == 0 {
		fmt.Fprintf(sb, "type %s { ok: Boolean! @goField(forceResolver: true) }\n", v.WrapperGoName)
		return
	}
	if len(fields) == 1 {
		fmt.Fprintf(sb, "type %s { %s }\n", v.WrapperGoName, fields[0])
		return
	}
	fmt.Fprintf(sb, "type %s {\n", v.WrapperGoName)
	for _, f := range fields {
		sb.WriteString("  ")
		sb.WriteString(f)
		sb.WriteString("\n")
	}
	sb.WriteString("}\n")
}

// inputGQLType returns the GraphQL type string for an input field.
func inputGQLType(field *protogen.Field, msgInfo map[string]*messageInfo) string {
	if field.Desc.IsMap() {
		// Maps are omitted from input types; caller must check.
		return ""
	}

	base := inputGQLBaseType(field, msgInfo)
	if base == "" {
		return ""
	}

	if field.Desc.IsList() {
		switch field.Desc.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			return fmt.Sprintf("[%s]!", base)
		default:
			return fmt.Sprintf("[%s!]!", base)
		}
	}

	// Singular.
	switch field.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return base // nullable nested input
	default:
		if field.Desc.HasOptionalKeyword() {
			return base
		}
		return base + "!"
	}
}

func inputGQLBaseType(field *protogen.Field, msgInfo map[string]*messageInfo) string {
	switch field.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		fqn := string(field.Desc.Message().FullName())
		if sc, ok := wellKnownGQLType[fqn]; ok {
			return sc
		}
		msgName := field.Message.GoIdent.GoName
		mi, ok := msgInfo[messageKey(field.Message)]
		if ok {
			if mi.isRequest {
				return inputTypeName(field.Message, msgInfo)
			}
			// Nested non-request messages get the "Input" suffix.
			return msgName + "Input"
		}
		return msgName
	case protoreflect.EnumKind:
		return string(field.Enum.GoIdent.GoName)
	default:
		return scalarForKind(field.Desc.Kind())
	}
}

// inputFields returns the renderable input fields for a message (omitting maps).
// oneofsByMsg maps messageKey to its oneof infos so oneof fields are replaced
// by the @oneOf input type reference.
func inputFields(msg *protogen.Message, msgInfo map[string]*messageInfo, oneofsByMsg map[string][]oneofInfo) []string {
	// Build a set of field names that belong to any non-synthetic oneof in this message.
	oneofFieldNames := map[string]bool{}
	oneofByProtoName := map[string]oneofInfo{}
	for _, oi := range oneofsByMsg[messageKey(msg)] {
		if oi.IsInput {
			for _, v := range oi.Variants {
				oneofFieldNames[v.ProtoFieldName] = true
			}
			oneofByProtoName[oi.ProtoName] = oi
		}
	}

	var lines []string
	emittedOneofs := map[string]bool{}

	for _, field := range msg.Fields {
		if field.Desc.IsMap() {
			continue
		}
		protoName := string(field.Desc.Name())

		// Check if this field belongs to an input oneof.
		if oneofFieldNames[protoName] {
			if field.Oneof != nil && !field.Oneof.Desc.IsSynthetic() {
				ooProtoName := string(field.Oneof.Desc.Name())
				if oi, ok := oneofByProtoName[ooProtoName]; ok && !emittedOneofs[ooProtoName] {
					emittedOneofs[ooProtoName] = true
					// Emit the @oneOf input reference (nullable — the whole oneof is optional).
					lines = append(lines, fmt.Sprintf("%s: %s", oi.GQLFieldName, oi.InputGQLName))
				}
			}
			continue
		}

		t := inputGQLType(field, msgInfo)
		if t == "" {
			continue
		}
		goFieldName := fieldName(protoName)
		lines = append(lines, fmt.Sprintf("%s: %s", goFieldName, t))
	}
	return lines
}

// emitInputTypes emits `input` blocks in the order prescribed by the spike:
// For each service method (in file order):
//  1. Emit the top-level request input (single-line if one field, multi-line otherwise).
//  2. Then emit @oneOf input types for any input oneofs in this request.
//  3. Then emit nested input types that were referenced from that request's fields
//     (depth-first, in field order).
//
// This matches the spike's ordering: GetBookRequest, AddBookRequest, BookInput,
// AuthorInput, WatchBooksRequest.
//
// Note: the spike emits AddBookRequest BEFORE BookInput even though BookInput is
// referenced by AddBookRequest. This is valid GraphQL (forward references are OK).
func emitInputTypes(sb *strings.Builder, f *protogen.File, msgInfo map[string]*messageInfo, oneofsByMsg map[string][]oneofInfo) error {
	return emitInputTypesGraph(sb, graphFromFile(f), msgInfo, oneofsByMsg)
}

func emitInputTypesGraph(sb *strings.Builder, g *graph, msgInfo map[string]*messageInfo, oneofsByMsg map[string][]oneofInfo) error {
	emitted := map[string]bool{}

	for _, svc := range g.Services {
		for _, m := range includedMethods(svc) {
			reqName := inputTypeName(m.Input, msgInfo)
			// Empty request messages have no GraphQL input type (the operation field has no arg).
			if !emitted[reqName] {
				emitted[reqName] = true
				if !isEmptyMessage(m.Input) {
					emitInputBlock(sb, m.Input, reqName, msgInfo, oneofsByMsg)
				}
			}
			// Emit @oneOf input blocks for input oneofs in this request.
			for _, oi := range oneofsByMsg[messageKey(m.Input)] {
				if !oi.IsInput {
					continue
				}
				oneofKey := oi.InputGQLName
				if !emitted[oneofKey] {
					emitted[oneofKey] = true
					emitOneofInputBlock(sb, oi, msgInfo)
				}
			}
			// Emit nested input types reachable from this request (DFS).
			if err := emitNestedInputs(sb, m.Input, msgInfo, oneofsByMsg, emitted); err != nil {
				return err
			}
		}
	}
	return nil
}

// emitNestedInputs emits nested input blocks reachable from msg (DFS).
// Nested types are emitted AFTER their parent request (not before).
// Returns an error if an empty (fieldless) nested input is encountered.
func emitNestedInputs(sb *strings.Builder, msg *protogen.Message, msgInfo map[string]*messageInfo, oneofsByMsg map[string][]oneofInfo, emitted map[string]bool) error {
	for _, field := range msg.Fields {
		if field.Desc.IsMap() || field.Desc.Kind() != protoreflect.MessageKind {
			continue
		}
		fqn := string(field.Desc.Message().FullName())
		if _, wkt := wellKnownGQLType[fqn]; wkt {
			continue
		}
		childName := field.Message.GoIdent.GoName
		mi, ok := msgInfo[messageKey(field.Message)]
		if !ok || !mi.role.has(roleInput) || mi.isRequest {
			continue
		}
		if emitted[childName] {
			continue
		}
		emitted[childName] = true
		// Fail-fast: empty nested inputs are not supported (they would produce
		// an invalid GraphQL input type and cannot be used as operation arguments).
		if isEmptyMessage(field.Message) {
			return fmt.Errorf("message %s is empty and used as a nested GraphQL input; empty nested inputs are not supported", childName)
		}
		emitInputBlock(sb, field.Message, childName+"Input", msgInfo, oneofsByMsg)
		// Recurse into nested types.
		if err := emitNestedInputs(sb, field.Message, msgInfo, oneofsByMsg, emitted); err != nil {
			return err
		}
	}
	return nil
}

// emitInputBlock emits a single `input` block.
// Single-field inputs use inline format; multi-field use multi-line.
func emitInputBlock(sb *strings.Builder, msg *protogen.Message, typeName string, msgInfo map[string]*messageInfo, oneofsByMsg map[string][]oneofInfo) {
	fields := inputFields(msg, msgInfo, oneofsByMsg)
	if len(fields) == 0 {
		fmt.Fprintf(sb, "input %s { _: Boolean }\n", typeName)
		return
	}
	if len(fields) == 1 {
		// Inline format.
		fmt.Fprintf(sb, "input %s { %s }\n", typeName, fields[0])
	} else {
		// Multi-line format.
		fmt.Fprintf(sb, "input %s {\n", typeName)
		for _, f := range fields {
			sb.WriteString("  ")
			sb.WriteString(f)
			sb.WriteString("\n")
		}
		sb.WriteString("}\n")
	}
}

// emitOneofInputBlock emits an `input @oneOf` block for a proto oneof field.
// The @oneOf input has one nullable field per oneof variant.
func emitOneofInputBlock(sb *strings.Builder, oi oneofInfo, msgInfo map[string]*messageInfo) {
	if len(oi.Variants) == 1 {
		v := oi.Variants[0]
		fmt.Fprintf(sb, "input %s @oneOf { %s: %s }\n", oi.InputGQLName, fieldName(v.ProtoFieldName), oneofInputVariantGQLType(v, msgInfo))
		return
	}
	fmt.Fprintf(sb, "input %s @oneOf {\n", oi.InputGQLName)
	for _, v := range oi.Variants {
		fmt.Fprintf(sb, "  %s: %s\n", fieldName(v.ProtoFieldName), oneofInputVariantGQLType(v, msgInfo))
	}
	sb.WriteString("}\n")
}

func oneofInputVariantGQLType(v oneofVariant, msgInfo map[string]*messageInfo) string {
	if !v.IsMessage || v.Msg == nil {
		return v.GQLTypeName
	}
	mi := msgInfo[messageKey(v.Msg)]
	if mi == nil {
		return v.GQLTypeName
	}
	if mi.isRequest {
		return inputTypeName(v.Msg, msgInfo)
	}
	return v.Msg.GoIdent.GoName + "Input"
}

// operationType determines whether a method maps to Query, Mutation, or
// Subscription. A graphqlopt.method.operation override takes precedence over
// the idempotency-based default. (Conflicts between a streaming shape and the
// override are validated separately in validateOperationOverrides.)
func operationType(m *protogen.Method) string {
	if o := methodOpts(m); o != nil {
		switch o.GetOperation() {
		case graphqlopt.Operation_QUERY:
			return "Query"
		case graphqlopt.Operation_MUTATION:
			return "Mutation"
		case graphqlopt.Operation_SUBSCRIPTION:
			return "Subscription"
		}
	}
	if m.Desc.IsStreamingServer() {
		return "Subscription"
	}
	opts, ok := m.Desc.Options().(*descriptorpb.MethodOptions)
	if ok && opts != nil {
		level := opts.GetIdempotencyLevel()
		if level == descriptorpb.MethodOptions_NO_SIDE_EFFECTS {
			return "Query"
		}
	}
	return "Mutation"
}

// methodFieldName returns the GraphQL operation field name for a method,
// honoring a graphqlopt.method.operation_name override.
func methodFieldName(m *protogen.Method) string {
	if o := methodOpts(m); o != nil && o.GetOperationName() != "" {
		return o.GetOperationName()
	}
	return operationFieldName(m.GoName)
}

// resolverMethodName returns the Go method name gqlgen will generate for this
// operation's resolver, i.e. the GraphQL field name with its first letter
// upper-cased. When no operation_name override is set this equals m.GoName.
func resolverMethodName(m *protogen.Method) string {
	name := methodFieldName(m)
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// isIdempotentMutation returns true iff the method is a Mutation with
// idempotency_level = IDEMPOTENT (not NO_SIDE_EFFECTS, not streaming).
func isIdempotentMutation(m *protogen.Method) bool {
	if m.Desc.IsStreamingServer() {
		return false
	}
	opts, ok := m.Desc.Options().(*descriptorpb.MethodOptions)
	if !ok || opts == nil {
		return false
	}
	return opts.GetIdempotencyLevel() == descriptorpb.MethodOptions_IDEMPOTENT
}

// hasAnyIdempotentMutation returns true iff f contains at least one
// method that should carry the @idempotent directive.
func hasAnyIdempotentMutation(f *protogen.File) bool {
	return hasAnyIdempotentMutationGraph(graphFromFile(f))
}

func hasAnyIdempotentMutationGraph(g *graph) bool {
	for _, svc := range g.Services {
		for _, m := range includedMethods(svc) {
			if isIdempotentMutation(m) {
				return true
			}
		}
	}
	return false
}

// emitOperationRoots emits Query, Mutation, Subscription root types.
func emitOperationRoots(sb *strings.Builder, f *protogen.File) {
	emitOperationRootsGraph(sb, graphFromFile(f))
}

func emitOperationRootsGraph(sb *strings.Builder, g *graph) {
	queryFields := []string{}
	mutationFields := []string{}
	subscriptionFields := []string{}
	msgInfo := analyzeMessagesGraph(g)

	for _, svc := range g.Services {
		for _, m := range includedMethods(svc) {
			opType := operationType(m)
			opField := methodFieldName(m)
			reqTypeName := inputTypeName(m.Input, msgInfo)
			emptyReq := isEmptyMessage(m.Input)

			switch opType {
			case "Query":
				retType := m.Output.GoIdent.GoName
				var f string
				if emptyReq {
					f = fmt.Sprintf("%s: %s!", opField, retType)
				} else {
					f = fmt.Sprintf("%s(input: %s!): %s!", opField, reqTypeName, retType)
				}
				queryFields = append(queryFields, f)
			case "Mutation":
				retType := m.Output.GoIdent.GoName
				var field string
				if emptyReq {
					field = fmt.Sprintf("%s: %s!", opField, retType)
				} else {
					field = fmt.Sprintf("%s(input: %s!): %s!", opField, reqTypeName, retType)
				}
				if isIdempotentMutation(m) {
					field += " @idempotent"
				}
				mutationFields = append(mutationFields, field)
			case "Subscription":
				streamType := m.Output.GoIdent.GoName
				var f string
				if emptyReq {
					f = fmt.Sprintf("%s: %s!", opField, streamType)
				} else {
					f = fmt.Sprintf("%s(input: %s!): %s!", opField, reqTypeName, streamType)
				}
				subscriptionFields = append(subscriptionFields, f)
			}
		}
	}

	if len(queryFields) > 0 {
		emitOpRoot(sb, "Query", queryFields)
	}
	if len(mutationFields) > 0 {
		emitOpRoot(sb, "Mutation", mutationFields)
	}
	if len(subscriptionFields) > 0 {
		emitOpRoot(sb, "Subscription", subscriptionFields)
	}
}

func emitOpRoot(sb *strings.Builder, name string, fields []string) {
	if len(fields) == 1 {
		fmt.Fprintf(sb, "type %s { %s }\n", name, fields[0])
	} else {
		fmt.Fprintf(sb, "type %s {\n", name)
		for _, f := range fields {
			sb.WriteString("  ")
			sb.WriteString(f)
			sb.WriteString("\n")
		}
		sb.WriteString("}\n")
	}
}
