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
	runtimePkg := "github.com/gopherex/protoc-gen-go-graphql/runtime"
	for _, sc := range []string{"Int64", "Uint64", "Bytes", "Timestamp", "Duration", "JSON"} {
		if usedScalars[sc] {
			fmt.Fprintf(&sb, "  %s:     { model: %s.%s }\n", sc, runtimePkg, sc)
		}
	}

	// Enum bindings → pbgql package.
	for _, e := range f.Enums {
		fmt.Fprintf(&sb, "  %s:        { model: %s.%s }\n", e.GoIdent.GoName, pbgqlImport, e.GoIdent.GoName)
	}

	// Message bindings.
	// We need:
	//   <MsgName>: { model: pbImport.<MsgName> }  — for output types
	//   <MsgName>Input: { model: pbImport.<MsgName> } — for nested input types
	//   <RequestName>: { model: pbImport.<RequestName> } — for top-level request inputs
	msgInfo := analyzeMessages(f)

	// Collect all file-level messages (not map entries).
	for _, msg := range f.Messages {
		if msg.Desc.IsMapEntry() {
			continue
		}
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
				// Request messages: already emitted above if also output, or emit fresh.
				if !mi.role.has(roleOutput) {
					fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", name, pbImport, name)
				}
			} else {
				// Nested input: emit with Input suffix binding to same Go type.
				fmt.Fprintf(&sb, "  %s:    { model: %s.%s }\n", name+"Input", pbImport, name)
			}
		}
	}

	// No `resolver:` block (spike-findings §1).
	sb.WriteString("\n")
	sb.WriteString("# No `resolver:` block on purpose: gqlgen then emits only the exec engine and the\n")
	sb.WriteString("# resolver INTERFACES (no stub files). The generator owns the resolver\n")
	sb.WriteString("# implementation that satisfies those interfaces and delegates to gRPC.\n")

	return sb.String()
}

