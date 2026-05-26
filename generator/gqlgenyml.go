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
	var sb strings.Builder

	// Schema section.
	sb.WriteString("schema:\n")
	sb.WriteString("  - schema.graphql\n")
	sb.WriteString("\n")

	// Exec section: relative to the gqlapi dir, exec is in exec/ subdirectory.
	sb.WriteString("exec:\n")
	sb.WriteString("  package: exec\n")
	sb.WriteString("  filename: exec/exec.go\n")
	sb.WriteString("\n")

	// Autobind: bind pb package's types.
	sb.WriteString("# Bind directly to the protoc-gen-go types; do NOT generate a second model set.\n")
	sb.WriteString("autobind:\n")
	fmt.Fprintf(&sb, "  - %s\n", pbImport)
	sb.WriteString("\n")

	// Models section.
	sb.WriteString("models:\n")

	// Scalar bindings (only for scalars actually used).
	usedScalars := collectUsedScalars(f)
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

	// Enum bindings → pbgql package.
	for _, e := range f.Enums {
		fmt.Fprintf(&sb, "  %s:        { model: %s.%s }\n", e.GoIdent.GoName, pbgqlImport, e.GoIdent.GoName)
	}

	// Collect oneof info for binding decisions.
	msgInfo := analyzeMessages(f)
	ois := collectOneofs(f, msgInfo)

	// Index oneofs by message name.
	// Messages with output oneofs: their oneof field uses a field resolver; the message itself still binds to pb.
	// Messages with input oneofs: the request message binds to an intermediate pbgql struct (not pb directly).
	inputOneofMsgs := map[string]oneofInfo{} // reqMsgGoName → oi
	outputOneofMsgs := map[string]bool{}     // msgGoName → true
	for _, oi := range ois {
		if oi.IsInput {
			inputOneofMsgs[oi.MsgGoName] = oi
		}
		if oi.IsOutput {
			outputOneofMsgs[oi.MsgGoName] = true
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
	for _, msg := range allMessages(f) {
		name := msg.GoIdent.GoName
		mi := msgInfo[name]
		if mi == nil {
			continue
		}

		// Emit output binding if used as output.
		if mi.role.has(roleOutput) {
			fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", name, pbImport, name)
		}

		// Emit input binding: top-level requests keep their name; nested get Input suffix.
		if mi.role.has(roleInput) {
			if mi.isRequest {
				if oi, hasInputOneof := inputOneofMsgs[name]; hasInputOneof {
					// Request with input oneof: bind to intermediate pbgql struct.
					fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", name, pbgqlImport, oi.MsgInputGoName)
				} else if !mi.role.has(roleOutput) {
					// Normal request without oneof: bind to pb directly.
					fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", name, pbImport, name)
				}
			} else {
				// Nested input: emit with Input suffix binding to same Go type.
				fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", name+"Input", pbImport, name)
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

	// No `resolver:` block (spike-findings §1).
	sb.WriteString("\n")
	sb.WriteString("# No `resolver:` block on purpose: gqlgen then emits only the exec engine and the\n")
	sb.WriteString("# resolver INTERFACES (no stub files). The generator owns the resolver\n")
	sb.WriteString("# implementation that satisfies those interfaces and delegates to gRPC.\n")

	return sb.String()
}
