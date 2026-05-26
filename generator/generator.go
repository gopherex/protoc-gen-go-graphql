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
	g.Settings.applyDefaults()
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
	// Ensure defaults even when Settings is constructed directly (e.g. in tests)
	// without going through RegisterFlags.
	g.Settings.applyDefaults()

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

	// OutDir is the configurable sub-package name (default: "gqlapi").
	outDir := g.Settings.OutDir

	// gqlapi lives as a sub-package next to the pb package dir.
	gqlapiImport := pbImport + "/" + outDir
	pbgqlImport := gqlapiImport + "/pbgql"
	execImport := gqlapiImport + "/exec"
	runtimeImport := "github.com/gopherex/protoc-gen-go-graphql/graphqlpb"

	// Base output directory for gqlapi files: source_relative means the proto
	// file's directory is used. The plugin writes files relative to the output root.
	// f.GeneratedFilenamePrefix gives the proto source path without extension,
	// e.g. "golden". We derive the gqlapi dir from it.
	protoDir := path.Dir(f.GeneratedFilenamePrefix)
	if protoDir == "." {
		protoDir = ""
	}
	gqlapiDir := joinPath(protoDir, outDir)

	// 1. schema.graphql (non-Go, write raw).
	schemaContent := buildSchema(f)
	schemaFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/schema.graphql", "")
	schemaFile.P(schemaContent)

	// 2. gqlgen.yml (non-Go, write raw).
	ymlContent := buildGqlgenYml(f, pbImport, pbgqlImport)
	ymlFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/gqlgen.yml", "")
	ymlFile.P(ymlContent)

	// 3. generate.go (Go source — //go:generate directive).
	genContent := buildGoGenerate(g.Settings.RunnerPkg, outDir)
	genFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/generate.go", protogen.GoImportPath(gqlapiImport))
	genFile.P(genContent)

	// 4. pbgql/<enum_lower>.go — enum adapters (including nested enums).
	for _, e := range allEnums(f) {
		adapterContent := buildEnumAdapter(e, pbImport)
		enumFileName := strings.ToLower(e.GoIdent.GoName)
		adapterFile := g.Plugin.NewGeneratedFile(
			gqlapiDir+"/pbgql/"+enumFileName+".go",
			protogen.GoImportPath(pbgqlImport),
		)
		adapterFile.P(adapterContent)
	}

	// 4b. pbgql/wkt_adapters.go — wrapper type scalar adapters.
	usedScalars := collectUsedScalars(f)
	wktContent := buildWKTAdapters(usedScalars)
	if wktContent != "" {
		wktFile := g.Plugin.NewGeneratedFile(
			gqlapiDir+"/pbgql/wkt_adapters.go",
			protogen.GoImportPath(pbgqlImport),
		)
		wktFile.P(wktContent)
	}

	// 4c. pbgql/<msg_lower>_oneof.go — oneof adapters (union wrappers + @oneOf input structs).
	msgInfo := analyzeMessages(f)
	ois := collectOneofs(f, msgInfo)
	// Group oneofs by message so we emit one file per message.
	oneofsByMsg := map[string][]oneofInfo{}
	for _, oi := range ois {
		oneofsByMsg[oi.MsgGoName] = append(oneofsByMsg[oi.MsgGoName], oi)
	}
	// Walk messages in DFS order to emit deterministically (incl. nested).
	for _, msg := range allMessages(f) {
		msgOis, ok := oneofsByMsg[msg.GoIdent.GoName]
		if !ok {
			continue
		}
		adapterContent := buildOneofAdapter(msg, msgOis, pbImport)
		if adapterContent == "" {
			continue
		}
		adapterFileName := strings.ToLower(msg.GoIdent.GoName) + "_oneof"
		adapterFile := g.Plugin.NewGeneratedFile(
			gqlapiDir+"/pbgql/"+adapterFileName+".go",
			protogen.GoImportPath(pbgqlImport),
		)
		adapterFile.P(adapterContent)
	}

	// 5. resolver.go.
	resolverContent := buildResolvers(f, outDir, pbImport, pbgqlImport, execImport, runtimeImport)
	resolverFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/resolver.go", protogen.GoImportPath(gqlapiImport))
	resolverFile.P(resolverContent)

	return nil
}

// joinPath joins path components, handling empty leading components.
func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}
