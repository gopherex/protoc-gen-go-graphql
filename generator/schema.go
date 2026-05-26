package generator

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
	descriptorpb "google.golang.org/protobuf/types/descriptorpb"
)

// wellKnownGQLType maps a fully qualified WKT message name to its GraphQL scalar name.
var wellKnownGQLType = map[string]string{
	"google.protobuf.Timestamp": "Timestamp",
	"google.protobuf.Duration":  "Duration",
	"google.protobuf.Struct":    "JSON",
	"google.protobuf.Value":     "JSON",
	"google.protobuf.ListValue": "JSON",
	"google.protobuf.Any":       "JSON",
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

// analyzeMessages returns a map from GoName to messageInfo for all non-map-entry
// messages in f. It determines which messages are used as output types, input
// types, or both.
func analyzeMessages(f *protogen.File) map[string]*messageInfo {
	info := map[string]*messageInfo{}

	// Initialize all top-level messages.
	var initAll func(msgs []*protogen.Message)
	initAll = func(msgs []*protogen.Message) {
		for _, msg := range msgs {
			if msg.Desc.IsMapEntry() {
				continue
			}
			info[msg.GoIdent.GoName] = &messageInfo{}
			initAll(msg.Messages)
		}
	}
	initAll(f.Messages)

	// Mark top-level request messages.
	for _, svc := range f.Services {
		for _, m := range svc.Methods {
			name := m.Input.GoIdent.GoName
			if mi, ok := info[name]; ok {
				mi.isRequest = true
				mi.role |= roleInput
			}
		}
	}

	// BFS from RPC responses → mark output.
	var markOutput func(msg *protogen.Message)
	markOutput = func(msg *protogen.Message) {
		name := msg.GoIdent.GoName
		mi, ok := info[name]
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
				childName := field.Message.GoIdent.GoName
				mi, ok := info[childName]
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

	for _, svc := range f.Services {
		for _, m := range svc.Methods {
			markOutput(m.Output)
			markInput(m.Input)
		}
	}

	return info
}

// buildSchema walks f's descriptors and returns a GraphQL SDL string.
// The output matches spike/schema.graphql for the golden proto:
//  1. @goField directive
//  2. Scalar declarations (used only)
//  3. Enum types
//  4. Domain output types (roleOutput & roleInput) — e.g. Author, Book
//  5. Input types (request messages and nested input types)
//  6. Pure output types (roleOutput & !roleInput) — e.g. GetBookResponse
//  7. Operation roots (Query / Mutation / Subscription)
func buildSchema(f *protogen.File) string {
	var sb strings.Builder

	msgInfo := analyzeMessages(f)

	// 1. @goField directive declaration (must be explicit per spike-findings §5).
	sb.WriteString("directive @goField(forceResolver: Boolean, name: String) on FIELD_DEFINITION | INPUT_FIELD_DEFINITION\n")
	sb.WriteString("\n")

	// 2. Scalar declarations (only those actually used).
	usedScalars := collectUsedScalars(f)
	for _, sc := range []string{"Int64", "Uint64", "Bytes", "Timestamp", "Duration", "JSON"} {
		if usedScalars[sc] {
			fmt.Fprintf(&sb, "scalar %s\n", sc)
		}
	}
	if len(usedScalars) > 0 {
		sb.WriteString("\n")
	}

	// 3. Enum types.
	for _, e := range f.Enums {
		emitEnum(&sb, e) // emitEnum already adds trailing blank line
	}

	// 4. Domain output types: roleOutput AND roleInput (used in both contexts).
	// E.g., Author and Book — used as output AND as templates for BookInput/AuthorInput.
	// Each may be single-line (Author) or multi-line (Book); add blank line after each.
	for _, msg := range f.Messages {
		if msg.Desc.IsMapEntry() {
			continue
		}
		mi := msgInfo[msg.GoIdent.GoName]
		if mi == nil {
			continue
		}
		if mi.role.has(roleOutput) && mi.role.has(roleInput) && !mi.isRequest {
			emitOutputType(&sb, msg)
			sb.WriteString("\n") // blank line after each domain type
		}
	}

	// 5. Input types: in service/method order (request messages) with nested types.
	emitInputTypes(&sb, f, msgInfo)
	sb.WriteString("\n") // blank line after input section

	// 6. Pure output types: roleOutput only (response wrappers like GetBookResponse).
	// Multiple single-line types appear consecutively without inter-item blank lines.
	for _, msg := range f.Messages {
		if msg.Desc.IsMapEntry() {
			continue
		}
		mi := msgInfo[msg.GoIdent.GoName]
		if mi == nil {
			continue
		}
		if mi.role.has(roleOutput) && !mi.role.has(roleInput) {
			emitOutputType(&sb, msg)
		}
	}
	sb.WriteString("\n") // blank line after response types section

	// 7. Operation roots.
	emitOperationRoots(&sb, f)

	return sb.String()
}

// collectUsedScalars scans every message field and returns the set of custom
// GraphQL scalar names (Int64, Uint64, Bytes, Timestamp, Duration, JSON) used.
func collectUsedScalars(f *protogen.File) map[string]bool {
	used := map[string]bool{}
	var scanMsg func(msg *protogen.Message)
	scanMsg = func(msg *protogen.Message) {
		for _, field := range msg.Fields {
			sc := fieldGQLScalar(field)
			if sc != "" {
				switch sc {
				case "Int64", "Uint64", "Bytes", "Timestamp", "Duration", "JSON":
					used[sc] = true
				}
			}
		}
		for _, nested := range msg.Messages {
			scanMsg(nested)
		}
	}
	for _, msg := range f.Messages {
		scanMsg(msg)
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
func outputFields(msg *protogen.Message) []string {
	var lines []string
	for _, field := range msg.Fields {
		var line string
		if field.Desc.IsMap() {
			gqlType := "JSON"
			goFieldName := fieldName(string(field.Desc.Name()))
			line = fmt.Sprintf("%s: %s @goField(forceResolver: true)", goFieldName, gqlType)
		} else {
			goFieldName := fieldName(string(field.Desc.Name()))
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
func emitOutputType(sb *strings.Builder, msg *protogen.Message) {
	if msg.Desc.IsMapEntry() {
		return
	}
	fields := outputFields(msg)
	if len(fields) == 1 {
		// Inline format.
		fmt.Fprintf(sb, "type %s { %s }\n", msg.GoIdent.GoName, fields[0])
	} else {
		// Multi-line format.
		fmt.Fprintf(sb, "type %s {\n", msg.GoIdent.GoName)
		for _, f := range fields {
			sb.WriteString("  " + f + "\n")
		}
		sb.WriteString("}\n")
	}
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
		mi, ok := msgInfo[msgName]
		if ok && !mi.isRequest {
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

// collectInputMessages returns the set of message GoNames that are reachable
// from RPC request messages as NESTED message types (not top-level requests).
func collectInputMessages(f *protogen.File) map[string]bool {
	msgInfo := analyzeMessages(f)
	result := map[string]bool{}
	for name, mi := range msgInfo {
		if mi.role.has(roleInput) && !mi.isRequest {
			result[name] = true
		}
	}
	return result
}

// inputFields returns the renderable input fields for a message (omitting maps).
func inputFields(msg *protogen.Message, msgInfo map[string]*messageInfo) []string {
	var lines []string
	for _, field := range msg.Fields {
		if field.Desc.IsMap() {
			continue
		}
		t := inputGQLType(field, msgInfo)
		if t == "" {
			continue
		}
		goFieldName := fieldName(string(field.Desc.Name()))
		lines = append(lines, fmt.Sprintf("%s: %s", goFieldName, t))
	}
	return lines
}

// emitInputTypes emits `input` blocks in the order prescribed by the spike:
// For each service method (in file order):
//   1. Emit the top-level request input (single-line if one field, multi-line otherwise).
//   2. Then emit nested input types that were referenced from that request's fields
//      (depth-first, in field order).
//
// This matches the spike's ordering: GetBookRequest, AddBookRequest, BookInput,
// AuthorInput, WatchBooksRequest.
//
// Note: the spike emits AddBookRequest BEFORE BookInput even though BookInput is
// referenced by AddBookRequest. This is valid GraphQL (forward references are OK).
func emitInputTypes(sb *strings.Builder, f *protogen.File, msgInfo map[string]*messageInfo) {
	emitted := map[string]bool{}

	for _, svc := range f.Services {
		for _, m := range svc.Methods {
			reqName := m.Input.GoIdent.GoName
			if !emitted[reqName] {
				emitted[reqName] = true
				emitInputBlock(sb, m.Input, reqName, msgInfo)
			}
			// Emit nested input types reachable from this request (DFS).
			emitNestedInputs(sb, m.Input, msgInfo, emitted)
		}
	}
}

// emitNestedInputs emits nested input blocks reachable from msg (DFS).
// Nested types are emitted AFTER their parent request (not before).
func emitNestedInputs(sb *strings.Builder, msg *protogen.Message, msgInfo map[string]*messageInfo, emitted map[string]bool) {
	for _, field := range msg.Fields {
		if field.Desc.IsMap() || field.Desc.Kind() != protoreflect.MessageKind {
			continue
		}
		fqn := string(field.Desc.Message().FullName())
		if _, wkt := wellKnownGQLType[fqn]; wkt {
			continue
		}
		childName := field.Message.GoIdent.GoName
		mi, ok := msgInfo[childName]
		if !ok || !mi.role.has(roleInput) || mi.isRequest {
			continue
		}
		if emitted[childName] {
			continue
		}
		emitted[childName] = true
		emitInputBlock(sb, field.Message, childName+"Input", msgInfo)
		// Recurse into nested types.
		emitNestedInputs(sb, field.Message, msgInfo, emitted)
	}
}

// emitInputBlock emits a single `input` block.
// Single-field inputs use inline format; multi-field use multi-line.
func emitInputBlock(sb *strings.Builder, msg *protogen.Message, typeName string, msgInfo map[string]*messageInfo) {
	fields := inputFields(msg, msgInfo)
	if len(fields) == 0 {
		fmt.Fprintf(sb, "input %s { }\n", typeName)
		return
	}
	if len(fields) == 1 {
		// Inline format.
		fmt.Fprintf(sb, "input %s { %s }\n", typeName, fields[0])
	} else {
		// Multi-line format.
		fmt.Fprintf(sb, "input %s {\n", typeName)
		for _, f := range fields {
			sb.WriteString("  " + f + "\n")
		}
		sb.WriteString("}\n")
	}
}

// operationType determines whether a method maps to Query, Mutation, or Subscription.
func operationType(m *protogen.Method) string {
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

// emitOperationRoots emits Query, Mutation, Subscription root types.
func emitOperationRoots(sb *strings.Builder, f *protogen.File) {
	queryFields := []string{}
	mutationFields := []string{}
	subscriptionFields := []string{}

	for _, svc := range f.Services {
		for _, m := range svc.Methods {
			opType := operationType(m)
			opField := operationFieldName(m.GoName)
			reqTypeName := m.Input.GoIdent.GoName

			switch opType {
			case "Query":
				retType := m.Output.GoIdent.GoName
				queryFields = append(queryFields,
					fmt.Sprintf("%s(input: %s!): %s!", opField, reqTypeName, retType))
			case "Mutation":
				retType := m.Output.GoIdent.GoName
				mutationFields = append(mutationFields,
					fmt.Sprintf("%s(input: %s!): %s!", opField, reqTypeName, retType))
			case "Subscription":
				streamType := m.Output.GoIdent.GoName
				subscriptionFields = append(subscriptionFields,
					fmt.Sprintf("%s(input: %s!): %s!", opField, reqTypeName, streamType))
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
			sb.WriteString("  " + f + "\n")
		}
		sb.WriteString("}\n")
	}
}
