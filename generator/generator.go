package generator

import (
	"path"
	"sort"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
)

type Generator struct {
	Plugin   *protogen.Plugin
	Settings *Settings
}

func New(p *protogen.Plugin, s *Settings) *Generator {
	return &Generator{Plugin: p, Settings: s}
}

func (g *Generator) Generate() error {
	g.Settings.applyDefaults()
	groups := map[string][]*protogen.File{}
	for _, f := range g.Plugin.Files {
		if !f.Generate {
			continue
		}
		groups[string(f.GoImportPath)] = append(groups[string(f.GoImportPath)], f)
	}
	var keys []string
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := g.generateFiles(groups[key]); err != nil {
			return err
		}
	}
	return nil
}

// generateFile is implemented across schema.go, gqlgenyml.go, resolvers.go.
func (g *Generator) generateFile(f *protogen.File) error {
	return g.generateFiles([]*protogen.File{f})
}

func (g *Generator) generateFiles(files []*protogen.File) error {
	if len(files) == 0 {
		return nil
	}
	f := files[0]
	// Ensure defaults even when Settings is constructed directly (e.g. in tests)
	// without going through RegisterFlags.
	g.Settings.applyDefaults()

	// Fail fast on unsupported streaming shapes. Skipped services and methods
	// are omitted entirely, so they are NOT checked (a skipped client/bidi rpc
	// must not error).
	for _, gf := range files {
		for _, svc := range gf.Services {
			if serviceSkipped(svc) {
				continue
			}
			for _, m := range svc.Methods {
				if methodSkipped(m) {
					continue
				}
				if err := checkStreaming(
					svc.GoName,
					m.GoName,
					m.Desc.IsStreamingClient(),
					m.Desc.IsStreamingServer(),
				); err != nil {
					return err
				}
			}
		}
	}
	graph := graphFromFiles(files)

	// Validate options: references to skipped messages, operation/idempotency
	// conflicts, and set-but-unimplemented options.
	if err := validateSkippedReferences(files); err != nil {
		return err
	}
	if err := validateOperationOverrides(graph); err != nil {
		return err
	}
	if err := validateUnsupportedOptions(graph); err != nil {
		return err
	}

	// Skip packages with no enabled GraphQL operations. A proto that defines only
	// messages (no services), or whose every service/method is skipped via
	// graphqlopt.service.skip / graphqlopt.method.skip, has no Query/Mutation/
	// Subscription root — there is nothing to serve. Emitting a gqlapi for it would
	// produce a rootless schema that gqlgen rejects (and, in single_pass mode, a
	// pointless `go list` over imported pb packages). Such message-only packages
	// are still imported and bound by the API packages that DO have services.
	if !graphHasOperations(graph) {
		return nil
	}

	// Derive import paths from the file descriptor.
	// f.GoImportPath is the pb package import path (e.g. "github.com/.../example/gen").
	pbImport := string(f.GoImportPath)
	// Strip the trailing package name suffix if present (e.g. ";gen").
	if idx := strings.Index(pbImport, ";"); idx >= 0 {
		pbImport = pbImport[:idx]
	}

	// FileOptions: pb_package override only (the graphql-go backend has no
	// gqlgen.yml / exec package to configure).
	if o := fileOpts(f); o != nil {
		if v := o.GetPbPackage(); v != "" {
			pbImport = v
		}
	}

	// OutDir is the configurable sub-package name + package name (default: "gqlapi").
	outDir := g.Settings.OutDir
	gqlapiImport := pbImport + "/" + outDir
	runtimeImport := "github.com/gopherex/protoc-gen-go-graphql/graphqlrt"

	// source_relative output dir derived from the proto file's directory.
	protoDir := path.Dir(f.GeneratedFilenamePrefix)
	if protoDir == "." {
		protoDir = ""
	}
	gqlapiDir := joinPath(protoDir, outDir)

	// One self-contained schema.go: builds the executable graphql.Schema whose
	// field resolvers delegate to the user's pb.*ServiceServer implementations.
	schemaContent, err := buildGraphQLGoGraph(graph, outDir, pbImport, runtimeImport)
	if err != nil {
		return err
	}
	schemaFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/schema.go", protogen.GoImportPath(gqlapiImport))
	schemaFile.P(schemaContent)

	return nil
}

// joinPath joins path components, handling empty leading components.
func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}
