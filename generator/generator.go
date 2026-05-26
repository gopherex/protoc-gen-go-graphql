package generator

import (
	"path"
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
	for _, f := range g.Plugin.Files {
		if !f.Generate {
			continue
		}
		if err := g.generateFile(f); err != nil {
			return err
		}
	}
	return nil
}

// generateFile is implemented across schema.go, gqlgenyml.go, resolvers.go.
func (g *Generator) generateFile(f *protogen.File) error {
	// Fail fast on unsupported streaming shapes.
	for _, svc := range f.Services {
		for _, m := range svc.Methods {
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

	// Derive import paths from the file descriptor.
	// f.GoImportPath is the pb package import path (e.g. "github.com/.../example/gen").
	pbImport := string(f.GoImportPath)
	// Strip the trailing package name suffix if present (e.g. ";gen").
	if idx := strings.Index(pbImport, ";"); idx >= 0 {
		pbImport = pbImport[:idx]
	}

	// gqlapi lives as a sub-package next to the pb package dir.
	gqlapiImport := pbImport + "/gqlapi"
	pbgqlImport := gqlapiImport + "/pbgql"
	execImport := gqlapiImport + "/exec"
	runtimeImport := "github.com/gopherex/protoc-gen-go-graphql/runtime"

	// Derive the module path (everything before the first path component that
	// corresponds to this module — we extract it from the pb import path).
	// For "github.com/gopherex/protoc-gen-go-graphql/example/gen" the module is
	// "github.com/gopherex/protoc-gen-go-graphql".
	modulePath := deriveModulePath(pbImport)

	// Base output directory for gqlapi files: source_relative means the proto
	// file's directory is used. The plugin writes files relative to the output root.
	// f.GeneratedFilenamePrefix gives the proto source path without extension,
	// e.g. "golden". We derive the gqlapi dir from it.
	protoDir := path.Dir(f.GeneratedFilenamePrefix)
	if protoDir == "." {
		protoDir = ""
	}
	gqlapiDir := joinPath(protoDir, "gqlapi")

	// 1. schema.graphql (non-Go, write raw).
	schemaContent := buildSchema(f)
	schemaFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/schema.graphql", "")
	schemaFile.P(schemaContent)

	// 2. gqlgen.yml (non-Go, write raw).
	ymlContent := buildGqlgenYml(f, pbImport, pbgqlImport)
	ymlFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/gqlgen.yml", "")
	ymlFile.P(ymlContent)

	// 3. generate.go (Go source — //go:generate directive).
	genContent := buildGoGenerate(modulePath)
	genFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/generate.go", protogen.GoImportPath(gqlapiImport))
	genFile.P(genContent)

	// 4. pbgql/<enum_lower>.go — enum adapters.
	for _, e := range f.Enums {
		adapterContent := buildEnumAdapter(e, pbImport)
		enumFileName := strings.ToLower(e.GoIdent.GoName)
		adapterFile := g.Plugin.NewGeneratedFile(
			gqlapiDir+"/pbgql/"+enumFileName+".go",
			protogen.GoImportPath(pbgqlImport),
		)
		adapterFile.P(adapterContent)
	}

	// 5. resolver.go.
	resolverContent := buildResolvers(f, pbImport, execImport, runtimeImport)
	resolverFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/resolver.go", protogen.GoImportPath(gqlapiImport))
	resolverFile.P(resolverContent)

	return nil
}

// deriveModulePath extracts the module root path from a pb import path by reading
// the go.mod file from the Plugin's file set. We use a simple heuristic: scan
// forward through path components and stop at the first "conventional" non-module
// component ("example", "gen", "internal", "cmd", "pkg", "api").
// For "github.com/gopherex/protoc-gen-go-graphql/example/gen" → "github.com/gopherex/protoc-gen-go-graphql".
func deriveModulePath(pbImport string) string {
	// Remove any build-tag suffix.
	if idx := strings.Index(pbImport, ";"); idx >= 0 {
		pbImport = pbImport[:idx]
	}
	parts := strings.Split(pbImport, "/")
	// The module path is everything before the first non-domain "conventional" component.
	// We consider any of these as non-module-path components:
	conventional := map[string]bool{
		"example": true, "gen": true, "internal": true, "cmd": true,
		"pkg": true, "api": true, "gqlapi": true, "pbgql": true,
	}
	// First 3 components are typically host/org/repo (e.g. github.com/gopherex/protoc-gen-go-graphql).
	for i := 3; i < len(parts); i++ {
		if conventional[parts[i]] {
			return strings.Join(parts[:i], "/")
		}
	}
	// Fallback: strip last two components.
	if len(parts) >= 2 {
		return strings.Join(parts[:len(parts)-2], "/")
	}
	return pbImport
}

// joinPath joins path components, handling empty leading components.
func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}
