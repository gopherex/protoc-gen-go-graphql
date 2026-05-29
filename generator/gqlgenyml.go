package generator

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
)

// buildGqlgenYml emits the gqlgen.yml for file f.
// pbImport is the Go import path of the generated pb package
// (e.g. "github.com/gopherex/protoc-gen-go-graphql/example/gen").
// pbgqlImport is the Go import path of the pbgql sub-package
// (e.g. "github.com/gopherex/protoc-gen-go-graphql/example/gen/gqlapi/pbgql").
func buildGqlgenYml(f *protogen.File, pbImport, pbgqlImport string) string {
	return buildGqlgenYmlGraph(graphFromFile(f), pbImport, pbgqlImport, "schema.graphql", "exec", "exec/exec.go")
}

func buildGqlgenYmlGraph(g *graph, pbImport, pbgqlImport, schemaFilename, execPackage, execFilename string) string {
	var sb strings.Builder

	// Schema section.
	sb.WriteString("schema:\n")
	fmt.Fprintf(&sb, "  - %s\n", schemaFilename)
	sb.WriteString("\n")

	// Exec section: relative to the gqlapi dir, exec is in exec/ subdirectory.
	sb.WriteString("exec:\n")
	fmt.Fprintf(&sb, "  package: %s\n", execPackage)
	fmt.Fprintf(&sb, "  filename: %s\n", execFilename)
	sb.WriteString("\n")

	// Autobind: bind pb package's types.
	sb.WriteString("# Bind directly to the protoc-gen-go types; do NOT generate a second model set.\n")
	sb.WriteString("autobind:\n")
	autobinds := []string{pbImport}
	seenAutobinds := map[string]bool{pbImport: true}
	for _, msg := range g.Messages {
		importPath := string(msg.GoIdent.GoImportPath)
		if importPath != "" && !seenAutobinds[importPath] {
			seenAutobinds[importPath] = true
			autobinds = append(autobinds, importPath)
		}
	}
	for _, importPath := range autobinds {
		fmt.Fprintf(&sb, "  - %s\n", importPath)
	}
	sb.WriteString("\n")

	// Models section.
	sb.WriteString("models:\n")

	// Scalar bindings (only for scalars actually used).
	usedScalars := collectUsedScalarsGraph(g)
	runtimePkg := "github.com/gopherex/protoc-gen-go-graphql/graphqlpb"
	// These scalars live in the runtime package.
	for _, sc := range []string{"Int64", "Uint64", "Bytes", "Timestamp", "Duration", "JSON"} {
		if usedScalars[sc] {
			fmt.Fprintf(&sb, "  %s:     { model: %s.%s }\n", sc, runtimePkg, sc)
		}
	}
	// FieldMask scalar lives in runtime (protojson adapter).
	if usedScalars["FieldMask"] {
		fmt.Fprintf(&sb, "  FieldMask:     { model: %s.FieldMask }\n", runtimePkg)
	}
	// Wrapper type scalars: per-wrapper adapters live in pbgql.
	for _, sc := range []string{
		"DoubleValue", "FloatValue", "Int32Value", "UInt32Value",
		"Int64Value", "UInt64Value", "BoolValue", "StringValue", "BytesValue",
	} {
		if usedScalars[sc] {
			fmt.Fprintf(&sb, "  %s:     { model: %s.%s }\n", sc, pbgqlImport, sc)
		}
	}

	// Enum bindings → pbgql package. The KEY is the GraphQL enum name (override
	// honored); the model stays keyed by the Go name so gqlgen resolves the
	// pbgql adapter Marshal/Unmarshal funcs by that Go type.
	for _, e := range g.Enums {
		fmt.Fprintf(&sb, "  %s:        { model: %s.%s }\n", gqlEnumName(e), pbgqlImport, e.GoIdent.GoName)
	}

	// Collect oneof info for binding decisions.
	msgInfo := analyzeMessagesGraph(g)
	ois := collectOneofsGraph(g, msgInfo)

	// Index oneofs by message name.
	// Messages with output oneofs: their oneof field uses a field resolver; the message itself still binds to pb.
	// Messages with input oneofs: the request message binds to an intermediate pbgql struct (not pb directly).
	inputOneofMsgs := map[string]oneofInfo{} // messageKey(reqMsg) → oi
	outputOneofMsgs := map[string]bool{}     // messageKey(msg) → true
	for _, oi := range ois {
		key := messageKey(oi.Msg)
		if oi.IsInput {
			inputOneofMsgs[key] = oi
		}
		if oi.IsOutput {
			outputOneofMsgs[key] = true
		}
	}

	// Message bindings.
	// We need:
	//   <MsgName>: { model: pbImport.<MsgName> }  — for output types (no input oneof)
	//   <MsgName>Input: { model: pbImport.<MsgName> } — for nested input types
	//   <RequestName>: { model: pbgqlImport.<RequestName>Input } — for requests with input oneof
	//   <RequestName>: { model: pbImport.<RequestName> } — for requests without input oneof
	// Additionally for union types:
	//   <UnionName>: { model: pbgqlImport.<UnionName> }  — union interface
	//   <WrapperName>: { model: pbgqlImport.<WrapperName> } — union member wrapper
	// And for @oneOf input types:
	//   <OneofInputName>: { model: pbgqlImport.<OneofInputName> }

	// Collect all messages (including nested — they become flat Go types).
	for _, msg := range g.Messages {
		// goName is the Go model type; gqlName is the GraphQL type name (the
		// MessageOptions.name override when set). Binding KEYS use gqlName;
		// models always reference the Go name.
		goName := msg.GoIdent.GoName
		gqlName := gqlTypeName(msg)
		mi := msgInfo[messageKey(msg)]
		if mi == nil {
			continue
		}

		// Emit output binding if used as output.
		if mi.role.has(roleOutput) {
			fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", gqlName, string(msg.GoIdent.GoImportPath), goName)
		}

		// Emit input binding: top-level requests keep their name; nested get Input suffix.
		if mi.role.has(roleInput) {
			if mi.isRequest {
				// Empty request messages have no GraphQL input type — skip binding.
				if isEmptyMessage(msg) {
					// no binding needed: the operation field has no input arg
				} else {
					inputName := inputTypeName(msg, msgInfo)
					if oi, hasInputOneof := inputOneofMsgs[messageKey(msg)]; hasInputOneof {
						// Request with input oneof: bind to intermediate pbgql struct.
						fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", inputName, pbgqlImport, oi.MsgInputGoName)
					} else if !mi.role.has(roleOutput) || inputName != gqlName {
						// Normal request without oneof: bind to pb directly.
						fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", inputName, string(msg.GoIdent.GoImportPath), goName)
					}
				}
			} else {
				// Nested input: emit with Input suffix binding to same Go type.
				fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", gqlName+"Input", string(msg.GoIdent.GoImportPath), goName)
			}
		}
	}

	// Oneof-specific bindings.
	for _, oi := range ois {
		if oi.IsOutput {
			// Union interface.
			fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", oi.UnionGQLName, pbgqlImport, oi.InterfaceGoName)
			// Union member wrappers.
			for _, v := range oi.Variants {
				fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", v.WrapperGoName, pbgqlImport, v.WrapperGoName)
			}
		}
		if oi.IsInput {
			// @oneOf input struct.
			fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", oi.InputGQLName, pbgqlImport, oi.InputGoName)
		}
	}

	// Directives block: only emit when custom directives with no runtime behavior are used.
	// @idempotent carries no runtime behavior and must be marked skip_runtime to avoid
	// gqlgen generating a DirectiveRoot method that requires an implementation.
	if hasAnyIdempotentMutationGraph(g) {
		sb.WriteString("\ndirectives:\n")
		sb.WriteString("  idempotent:\n")
		sb.WriteString("    skip_runtime: true\n")
	}

	// No `resolver:` block (spike-findings §1).
	sb.WriteString("\n")
	sb.WriteString("# No `resolver:` block on purpose: gqlgen then emits only the exec engine and the\n")
	sb.WriteString("# resolver INTERFACES (no stub files). The generator owns the resolver\n")
	sb.WriteString("# implementation that satisfies those interfaces and delegates to gRPC.\n")

	return sb.String()
}
