package generator

// singlepass.go: in-plugin gqlgen execution for the single_pass opt-in mode.
// When Settings.SinglePass is true, generateFile calls runSinglePass instead of
// emitting a generate.go file. runSinglePass:
//  1. Writes the same gqlapi artifacts to a throwaway /tmp module.
//  2. Shells out protoc-gen-go to produce pb .go files inside the tmp module.
//  3. Copies graphqlpb sources into tmp when the user's module IS our own module.
//  4. Runs gqlgen in-process with SkipValidation+SkipModTidy, CWD = tmp gqlapi dir.
//  5. Reads exec/exec.go + models_gen.go back and emits them via the plugin response.

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/99designs/gqlgen/api"
	"github.com/99designs/gqlgen/codegen/config"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	descriptorpb "google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// runSinglePass is the opt-in single-pass gqlgen path. Called from generateFile
// when Settings.SinglePass == true.
//
// Parameters mirror what generateFile has already computed:
//   - gqlapiDir:    output path prefix for gqlapi files (relative to protoc output root)
//   - pbImport:     Go import path of the pb package
//   - pbgqlImport:  Go import path of the pbgql sub-package
//   - schemaContent: already-built schema.graphql string
//   - ymlContent:   already-built gqlgen.yml string
//   - pbgqlFiles:   map[filename]content for all pbgql/*.go files
//   - resolverContent: already-built resolver.go string
//
// It emits exec/exec.go and models_gen.go (relative to gqlapiDir) via the plugin.
func (g *Generator) runSinglePass(
	gqlapiDir string,
	pbImport string,
	pbgqlImport string,
	schemaContent string,
	ymlContent string,
	pbgqlFiles map[string]string, // filename (e.g. "genre.go") → content
	resolverContent string,
) error {
	// --- 1. Find the user's module (go.mod walk-up from CWD). ---
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("single_pass: getwd: %w", err)
	}
	goModDir, modulePath, err := findGoMod(cwd)
	if err != nil {
		return fmt.Errorf("single_pass: cannot locate go.mod (walk from %s): %w", cwd, err)
	}

	// --- 2. Compute relative pb package dir inside the module. ---
	// pbImport = "github.com/gopherex/protoc-gen-go-graphql/example/gen"
	// modulePath = "github.com/gopherex/protoc-gen-go-graphql"
	// rel = "example/gen"
	modulePrefix := modulePath + "/"
	if !strings.HasPrefix(pbImport, modulePrefix) {
		return fmt.Errorf("single_pass: pb import path %q does not start with module path %q/", pbImport, modulePath)
	}
	pbRelDir := strings.TrimPrefix(pbImport, modulePrefix) // e.g. "example/gen"

	// --- 3. Create throwaway tmp module dir. ---
	tmpRoot, err := os.MkdirTemp("", "protoc-gen-go-graphql-singlepass-*")
	if err != nil {
		return fmt.Errorf("single_pass: mkdtemp: %w", err)
	}
	defer os.RemoveAll(tmpRoot)

	// --- 4. Copy go.mod + go.sum into tmp root. ---
	if err := copyFile(filepath.Join(goModDir, "go.mod"), filepath.Join(tmpRoot, "go.mod")); err != nil {
		return fmt.Errorf("single_pass: copy go.mod: %w", err)
	}
	goSumSrc := filepath.Join(goModDir, "go.sum")
	if _, serr := os.Stat(goSumSrc); serr == nil {
		if err := copyFile(goSumSrc, filepath.Join(tmpRoot, "go.sum")); err != nil {
			return fmt.Errorf("single_pass: copy go.sum: %w", err)
		}
	}
	if err := prepareSinglePassGoMod(tmpRoot); err != nil {
		return fmt.Errorf("single_pass: prepare go.mod: %w", err)
	}
	modDownload := exec.Command("go", "mod", "download", "all")
	modDownload.Dir = tmpRoot
	if out, err := modDownload.CombinedOutput(); err != nil {
		return fmt.Errorf("single_pass: go mod download: %w\n%s", err, out)
	}

	// --- 5. Shell protoc-gen-go to produce pb .go files. ---
	pbGenGoPath, err := exec.LookPath("protoc-gen-go")
	if err != nil {
		return fmt.Errorf("single_pass: protoc-gen-go not found on PATH (install with: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest): %w", err)
	}

	// Collect all FileDescriptorProtos needed: the files being generated plus all
	// their transitive imports (deduped by name).
	var allProtoFiles []*descriptorpb.FileDescriptorProto
	var fileToGenerate []string
	seenNames := map[string]bool{}

	// Collect all files transitively imported (DFS), dependencies before dependents.
	var collectFile func(pf *protogen.File)
	collectFile = func(pf *protogen.File) {
		name := pf.Desc.Path()
		if seenNames[name] {
			return
		}
		seenNames[name] = true
		// Walk imports first (dependencies before dependents).
		imports := pf.Desc.Imports()
		for i := 0; i < imports.Len(); i++ {
			imp := imports.Get(i)
			// Find the corresponding protogen.File.
			for _, pluginFile := range g.Plugin.Files {
				if pluginFile.Desc.Path() == imp.Path() {
					collectFile(pluginFile)
					break
				}
			}
			// If not found in plugin.Files (e.g. WKT), still include the descriptor.
			if !seenNames[imp.Path()] {
				seenNames[imp.Path()] = true
				allProtoFiles = append(allProtoFiles, protodesc.ToFileDescriptorProto(imp))
			}
		}
		allProtoFiles = append(allProtoFiles, protodesc.ToFileDescriptorProto(pf.Desc))
	}

	// Collect deps for every file-to-generate; track which are generated.
	for _, pluginFile := range g.Plugin.Files {
		collectFile(pluginFile)
	}
	for _, pluginFile := range g.Plugin.Files {
		if pluginFile.Generate {
			fileToGenerate = append(fileToGenerate, pluginFile.Desc.Path())
		}
	}

	// Build a CodeGeneratorRequest and pipe it to protoc-gen-go.
	req := &pluginpb.CodeGeneratorRequest{
		ProtoFile:      allProtoFiles,
		FileToGenerate: fileToGenerate,
		Parameter:      proto.String("paths=source_relative"),
	}
	reqBytes, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("single_pass: marshal protoc-gen-go request: %w", err)
	}

	pgCmd := exec.Command(pbGenGoPath)
	pgCmd.Stdin = bytes.NewReader(reqBytes)
	pgOut, err := pgCmd.Output()
	if err != nil {
		return fmt.Errorf("single_pass: protoc-gen-go failed: %w", err)
	}

	var pgResp pluginpb.CodeGeneratorResponse
	if err := proto.Unmarshal(pgOut, &pgResp); err != nil {
		return fmt.Errorf("single_pass: unmarshal protoc-gen-go response: %w", err)
	}
	if pgResp.Error != nil {
		return fmt.Errorf("single_pass: protoc-gen-go returned error: %s", pgResp.GetError())
	}

	// Write each pb .go file under the module directory matching the source
	// file's Go import path. With multiple proto packages in one request,
	// source_relative response names cannot all be placed under this graph's
	// pbRelDir.
	prefixToFile := map[string]*protogen.File{}
	for _, pluginFile := range g.Plugin.Files {
		prefixToFile[pluginFile.GeneratedFilenamePrefix] = pluginFile
	}
	for _, respFile := range pgResp.File {
		respName := respFile.GetName()
		respPrefix := strings.TrimSuffix(respName, ".pb.go")
		pluginFile := prefixToFile[respPrefix]
		if pluginFile == nil {
			return fmt.Errorf("single_pass: cannot map protoc-gen-go output %s to a proto file", respName)
		}
		importPath := string(pluginFile.GoImportPath)
		if !strings.HasPrefix(importPath, modulePrefix) {
			return fmt.Errorf("single_pass: generated pb import path %q does not start with module path %q/", importPath, modulePath)
		}
		relImportDir := strings.TrimPrefix(importPath, modulePrefix)
		dest := filepath.Join(tmpRoot, relImportDir, filepath.Base(filepath.FromSlash(respName)))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("single_pass: mkdir for pb file %s: %w", respName, err)
		}
		if err := os.WriteFile(dest, []byte(respFile.GetContent()), 0o644); err != nil {
			return fmt.Errorf("single_pass: write pb file %s: %w", respName, err)
		}
	}

	// --- 6. Write gqlapi artifacts into tmp. ---
	// gqlapi dir: tmp/<pbRelDir>/gqlapi/
	outDir := g.Settings.OutDir
	tmpGqlapiDir := filepath.Join(tmpRoot, pbRelDir, outDir)
	tmpPbgqlDir := filepath.Join(tmpGqlapiDir, "pbgql")
	if err := os.MkdirAll(tmpGqlapiDir, 0o755); err != nil {
		return fmt.Errorf("single_pass: mkdir gqlapi: %w", err)
	}
	if err := os.MkdirAll(tmpPbgqlDir, 0o755); err != nil {
		return fmt.Errorf("single_pass: mkdir pbgql: %w", err)
	}

	if err := os.WriteFile(filepath.Join(tmpGqlapiDir, "schema.graphql"), []byte(schemaContent), 0o644); err != nil {
		return fmt.Errorf("single_pass: write schema.graphql: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpGqlapiDir, "gqlgen.yml"), []byte(ymlContent), 0o644); err != nil {
		return fmt.Errorf("single_pass: write gqlgen.yml: %w", err)
	}

	// pbgql files.
	for name, content := range pbgqlFiles {
		if err := os.WriteFile(filepath.Join(tmpPbgqlDir, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("single_pass: write pbgql/%s: %w", name, err)
		}
	}

	// resolver.go — needed for gqlgen to see the resolver type; write it.
	if err := os.WriteFile(filepath.Join(tmpGqlapiDir, "resolver.go"), []byte(resolverContent), 0o644); err != nil {
		return fmt.Errorf("single_pass: write resolver.go: %w", err)
	}

	// --- 7. Copy graphqlpb if this is our own module. ---
	graphqlpbImport := "github.com/gopherex/protoc-gen-go-graphql/graphqlpb"
	if strings.HasPrefix(graphqlpbImport, modulePrefix) {
		// graphqlpb is inside the user's module (true for this repo itself).
		graphqlpbRelDir := strings.TrimPrefix(graphqlpbImport, modulePrefix)
		graphqlpbSrc := filepath.Join(goModDir, graphqlpbRelDir)
		graphqlpbDst := filepath.Join(tmpRoot, graphqlpbRelDir)
		if err := copyDir(graphqlpbSrc, graphqlpbDst); err != nil {
			return fmt.Errorf("single_pass: copy graphqlpb: %w", err)
		}
	}

	preloadTargets := []string{pbImport}
	if len(pbgqlFiles) > 0 {
		preloadTargets = append(preloadTargets, pbgqlImport)
	}
	preloadArgs := append([]string{"list", "-deps", "-mod=mod"}, preloadTargets...)
	preload := exec.Command("go", preloadArgs...)
	preload.Dir = tmpRoot
	if out, err := preload.CombinedOutput(); err != nil {
		return fmt.Errorf("single_pass: go list deps: %w\n%s", err, out)
	}

	// --- 8. Run gqlgen in-process with CWD = tmp gqlapi dir. ---
	// Save and restore CWD.
	origDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("single_pass: getwd before chdir: %w", err)
	}
	if err := os.Chdir(tmpGqlapiDir); err != nil {
		return fmt.Errorf("single_pass: chdir to tmp gqlapi dir: %w", err)
	}
	defer func() { _ = os.Chdir(origDir) }()

	cfg, err := config.LoadConfig("gqlgen.yml")
	if err != nil {
		return fmt.Errorf("single_pass: gqlgen config.LoadConfig: %w", err)
	}
	cfg.SkipValidation = true
	cfg.SkipModTidy = true
	cfg.OmitRootModels = true

	if err := api.Generate(cfg); err != nil {
		return fmt.Errorf("single_pass: gqlgen api.Generate: %w", err)
	}

	// Restore CWD before reading files (we have a defer, but restore early for
	// clarity — the defer is a safety net).
	if err := os.Chdir(origDir); err != nil {
		return fmt.Errorf("single_pass: chdir back: %w", err)
	}

	// --- 9. Read gqlgen outputs and emit via plugin response. ---
	// exec filename comes from gqlgen.yml: exec.filename = "exec/exec.go"
	execGoPath := filepath.Join(tmpGqlapiDir, "exec", "exec.go")
	execContent, err := os.ReadFile(execGoPath)
	if err != nil {
		return fmt.Errorf("single_pass: read generated exec/exec.go: %w", err)
	}

	// Emit exec/exec.go.
	execFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/exec/exec.go", protogen.GoImportPath(pbgqlImport+"/exec"))
	execFile.P(string(execContent))

	// Emit models_gen.go when gqlgen produced one. With OmitRootModels=true and
	// full proto autobinding, gqlgen may not need a model file at all.
	modelsGenPath := filepath.Join(tmpGqlapiDir, "models_gen.go")
	if modelsContent, err := os.ReadFile(modelsGenPath); err == nil {
		gqlapiImport := pbImport + "/" + outDir
		modelsFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/models_gen.go", protogen.GoImportPath(gqlapiImport))
		modelsFile.P(string(modelsContent))
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("single_pass: read generated models_gen.go: %w", err)
	}

	return nil
}

// pluginModulePath is the module that owns graphqlpb (and this plugin).
const pluginModulePath = "github.com/gopherex/protoc-gen-go-graphql"

// prepareSinglePassGoMod injects the deps the synthetic /tmp module needs to
// type-check pb + pbgql for gqlgen: gqlgen itself and graphqlpb.
//
// graphqlpb is pinned to the plugin's OWN module version. When the plugin runs
// as a published/installed module (real semver), that require resolves from the
// proxy/cache — no replace needed. When the plugin runs from a source checkout
// (version "(devel)", e.g. `go build`/CI/dev), there is no published version, so
// we fall back to a local `replace` pointing at the plugin's source tree.
//
// Uses `go mod edit` (not raw append) so it is idempotent against a go.mod that
// already requires these modules.
func prepareSinglePassGoMod(tmpRoot string) error {
	gqlgenVersion := moduleVersion("github.com/99designs/gqlgen")
	if gqlgenVersion == "" {
		gqlgenVersion = "v0.17.90"
	}
	if err := goModEdit(tmpRoot, "-require=github.com/99designs/gqlgen@"+gqlgenVersion); err != nil {
		return err
	}

	if v := pluginModuleVersion(); isRealVersion(v) {
		// Published: a normal versioned require, resolvable from the module proxy.
		return goModEdit(tmpRoot, "-require="+pluginModulePath+"@"+v)
	}

	// Dev/source build: no published version — replace with the local source tree.
	root, err := currentModuleRoot()
	if err != nil {
		return err
	}
	if err := goModEdit(tmpRoot, "-require="+pluginModulePath+"@v0.0.0"); err != nil {
		return err
	}
	return goModEdit(tmpRoot, "-replace="+pluginModulePath+"="+filepath.ToSlash(root))
}

// goModEdit runs `go mod edit <arg>` in dir.
func goModEdit(dir, arg string) error {
	cmd := exec.Command("go", "mod", "edit", arg)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go mod edit %s: %w\n%s", arg, err, out)
	}
	return nil
}

// pluginModuleVersion returns the version of the plugin's own (main) module, or
// "" if unavailable.
func pluginModuleVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return ""
}

// isRealVersion reports whether v is a resolvable published version (not a
// source/dev build).
func isRealVersion(v string) bool {
	return v != "" && v != "(devel)"
}

func moduleVersion(path string) string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == path {
				if dep.Replace != nil {
					return dep.Replace.Version
				}
				return dep.Version
			}
		}
	}
	return ""
}

func currentModuleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("cannot locate generator module root from %s", file)
}

// findGoMod walks up from startDir until it finds a go.mod file. Returns the
// directory containing go.mod and the module path declared in it.
func findGoMod(startDir string) (dir string, modulePath string, err error) {
	d := startDir
	for {
		goModPath := filepath.Join(d, "go.mod")
		if data, e := os.ReadFile(goModPath); e == nil {
			// Parse "module <path>" from the go.mod content.
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "module ") {
					mp := strings.TrimSpace(strings.TrimPrefix(line, "module "))
					// Strip any inline comment.
					if idx := strings.Index(mp, "//"); idx >= 0 {
						mp = strings.TrimSpace(mp[:idx])
					}
					return d, mp, nil
				}
			}
			return "", "", fmt.Errorf("go.mod at %s has no module directive", goModPath)
		}
		parent := filepath.Dir(d)
		if parent == d {
			break // reached filesystem root
		}
		d = parent
	}
	return "", "", fmt.Errorf("no go.mod found walking up from %s", startDir)
}

// copyFile copies src to dst (creates dst's parent dirs as needed).
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// copyDir recursively copies srcDir to dstDir, skipping *_test.go files.
func copyDir(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, srcPath)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dstPath, 0o755)
		}
		// Skip test files (they'd try to import test-only deps).
		if strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		return copyFile(srcPath, dstPath)
	})
}
