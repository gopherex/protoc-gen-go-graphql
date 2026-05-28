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

	// Fail fast on unsupported streaming shapes.
	for _, gf := range files {
		for _, svc := range gf.Services {
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
	}
	graph := graphFromFiles(files)

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
	schemaContent, err := buildSchemaGraph(graph)
	if err != nil {
		return err
	}
	schemaFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/schema.graphql", "")
	schemaFile.P(schemaContent)

	// 2. gqlgen.yml (non-Go, write raw).
	ymlContent := buildGqlgenYmlGraph(graph, pbImport, pbgqlImport)
	ymlFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/gqlgen.yml", "")
	ymlFile.P(ymlContent)

	// 3. generate.go (Go source — //go:generate directive).
	// In single_pass mode we skip this file (gqlgen runs inside the plugin).
	if !g.Settings.SinglePass {
		genContent := buildGoGenerate(g.Settings.RunnerPkg, outDir)
		genFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/generate.go", protogen.GoImportPath(gqlapiImport))
		genFile.P(genContent)
	}

	// 4. pbgql/<enum_lower>.go — enum adapters (including nested enums).
	// In single_pass mode we also collect the content for the tmp module.
	pbgqlFiles := map[string]string{} // filename → content (used by runSinglePass)
	for _, e := range graph.Enums {
		adapterContent := buildEnumAdapter(e, string(e.GoIdent.GoImportPath))
		enumFileName := strings.ToLower(e.GoIdent.GoName) + ".go"
		adapterFile := g.Plugin.NewGeneratedFile(
			gqlapiDir+"/pbgql/"+enumFileName,
			protogen.GoImportPath(pbgqlImport),
		)
		adapterFile.P(adapterContent)
		if g.Settings.SinglePass {
			pbgqlFiles[enumFileName] = adapterContent
		}
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
		if g.Settings.SinglePass {
			pbgqlFiles["wkt_adapters.go"] = wktContent
		}
	}

	// 4c. pbgql/<msg_lower>_oneof.go — oneof adapters (union wrappers + @oneOf input structs).
	msgInfo := analyzeMessagesGraph(graph)
	ois := collectOneofsGraph(graph, msgInfo)
	// Group oneofs by message so we emit one file per message.
	oneofsByMsg := map[string][]oneofInfo{}
	for _, oi := range ois {
		key := messageKey(oi.Msg)
		oneofsByMsg[key] = append(oneofsByMsg[key], oi)
	}
	// Walk messages in DFS order to emit deterministically (incl. nested).
	for _, msg := range graph.Messages {
		msgOis, ok := oneofsByMsg[messageKey(msg)]
		if !ok {
			continue
		}
		adapterContent := buildOneofAdapter(msgOis, string(msg.GoIdent.GoImportPath))
		if adapterContent == "" {
			continue
		}
		adapterFileName := strings.ToLower(msg.GoIdent.GoName) + "_oneof.go"
		adapterFile := g.Plugin.NewGeneratedFile(
			gqlapiDir+"/pbgql/"+adapterFileName,
			protogen.GoImportPath(pbgqlImport),
		)
		adapterFile.P(adapterContent)
		if g.Settings.SinglePass {
			pbgqlFiles[adapterFileName] = adapterContent
		}
	}

	// 5. resolver.go.
	resolverContent := buildResolversGraph(graph, outDir, pbImport, pbgqlImport, execImport, runtimeImport)
	resolverFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/resolver.go", protogen.GoImportPath(gqlapiImport))
	resolverFile.P(resolverContent)

	// 6. Single-pass: run gqlgen inside the plugin and emit exec + models_gen.
	if g.Settings.SinglePass {
		if err := g.runSinglePass(
			gqlapiDir,
			pbImport,
			pbgqlImport,
			schemaContent,
			ymlContent,
			pbgqlFiles,
			resolverContent,
		); err != nil {
			return err
		}
	}

	return nil
}

// joinPath joins path components, handling empty leading components.
func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}
