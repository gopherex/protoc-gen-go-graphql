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
	"path"
	"path/filepath"
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
//   - f:            the proto file being generated
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
	f *protogen.File,
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

	// Write each pb .go file under tmp/<pbRelDir>/<filename>.
	// protoc-gen-go with paths=source_relative emits filenames relative to the proto
	// file location. We place them at tmp/<pbRelDir>/<basename>.
	tmpPbDir := filepath.Join(tmpRoot, pbRelDir)
	if err := os.MkdirAll(tmpPbDir, 0o755); err != nil {
		return fmt.Errorf("single_pass: mkdir pb dir: %w", err)
	}
	for _, respFile := range pgResp.File {
		// The filename from protoc-gen-go is relative to the proto file dir.
		// Since we're using source_relative, it's just "<base>.pb.go" or
		// "<base>_grpc.pb.go" possibly with subdirs if proto is in a subdir.
		// We write them under tmpPbDir preserving the relative path.
		dest := filepath.Join(tmpPbDir, filepath.FromSlash(respFile.GetName()))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("single_pass: mkdir for pb file %s: %w", respFile.GetName(), err)
		}
		if err := os.WriteFile(dest, []byte(respFile.GetContent()), 0o644); err != nil {
			return fmt.Errorf("single_pass: write pb file %s: %w", respFile.GetName(), err)
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

	modelsGenPath := filepath.Join(tmpGqlapiDir, "models_gen.go")
	modelsContent, err := os.ReadFile(modelsGenPath)
	if err != nil {
		return fmt.Errorf("single_pass: read generated models_gen.go: %w", err)
	}

	// Emit exec/exec.go.
	execFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/exec/exec.go", protogen.GoImportPath(pbgqlImport+"/exec"))
	execFile.P(string(execContent))

	// Emit models_gen.go.
	// gqlapiImport = pbImport + "/" + outDir
	gqlapiImport := pbImport + "/" + outDir
	modelsFile := g.Plugin.NewGeneratedFile(gqlapiDir+"/models_gen.go", protogen.GoImportPath(gqlapiImport))
	modelsFile.P(string(modelsContent))

	return nil
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

// pbgqlFilename returns the output filename for a pbgql file (just the basename).
func pbgqlFilename(name string) string {
	return path.Base(name)
}
