package generator

// Integration test for buildSchema. Rather than unit-constructing a
// *protogen.File (which requires driving protoc's wire protocol), this test
// shells out to protoc to compile golden.proto into a FileDescriptorSet, loads
// it via protodesc + protogen, calls buildSchema, and compares the result to
// generator/testdata/golden.schema.graphql (normalized: trailing whitespace
// stripped, trailing newlines collapsed to one).
//
// Requirements: `protoc` and WKT includes must be available. The test is
// skipped if protoc is not on PATH.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// repoRoot returns the absolute path of the repo root (two dirs up from this
// file's directory, which is generator/).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file = .../generator/schema_test.go
	return filepath.Dir(filepath.Dir(file))
}

func normalizeSchema(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		out = append(out, strings.TrimRight(l, " \t"))
	}
	result := strings.Join(out, "\n")
	// Collapse multiple trailing newlines to one.
	result = strings.TrimRight(result, "\n") + "\n"
	return result
}

// loadGoldenProtoFile compiles golden.proto with protoc and returns the parsed
// *protogen.File. The test is skipped if protoc is not on PATH.
func loadGoldenProtoFile(t *testing.T) *protogen.File {
	t.Helper()

	if _, err := exec.LookPath("protoc"); err != nil {
		t.Skip("protoc not found on PATH, skipping golden test")
	}

	root := repoRoot(t)
	exampleDir := filepath.Join(root, "example")
	wktInc := "/usr/include"

	tmp, err := os.CreateTemp("", "golden-*.pb")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	cmd := exec.Command("protoc",
		"-I", exampleDir,
		"-I", root,
		"-I", wktInc,
		"--include_imports",
		"--descriptor_set_out="+tmp.Name(),
		filepath.Join(exampleDir, "golden.proto"),
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("protoc failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read descriptor set: %v", err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		t.Fatalf("unmarshal FileDescriptorSet: %v", err)
	}

	var fileToGen []string
	for _, fd := range fds.File {
		if fd.GetName() == "golden.proto" {
			fileToGen = append(fileToGen, fd.GetName())
		}
	}
	if len(fileToGen) == 0 {
		t.Fatal("golden.proto not found in descriptor set")
	}

	req := &pluginpb.CodeGeneratorRequest{
		ProtoFile:      fds.File,
		FileToGenerate: fileToGen,
		Parameter:      proto.String("paths=source_relative"),
	}

	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}

	for _, f := range plugin.Files {
		if f.Desc.Path() == "golden.proto" {
			return f
		}
	}
	t.Fatal("golden.proto not found in plugin.Files")
	return nil
}

// readTestdata reads a file from generator/testdata/.
func readTestdata(t *testing.T, name string) string {
	t.Helper()
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "generator", "testdata", name))
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return string(data)
}

// writeTestdata writes a file to generator/testdata/ (used when TESTDATA_UPDATE=1).
func writeTestdata(t *testing.T, name, content string) {
	t.Helper()
	root := repoRoot(t)
	path := filepath.Join(root, "generator", "testdata", name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write testdata %s: %v", name, err)
	}
	t.Logf("updated testdata/%s", name)
}

// testdataUpdateMode returns true when TESTDATA_UPDATE=1 is set.
func testdataUpdateMode() bool {
	return os.Getenv("TESTDATA_UPDATE") == "1"
}

// TestBuildSchema_IdempotentDirective asserts that the schema contains the
// @idempotent directive declaration and that the upsertBook mutation field
// carries the @idempotent annotation.
func TestBuildSchema_IdempotentDirective(t *testing.T) {
	goldenFile := loadGoldenProtoFile(t)
	schema := buildSchema(goldenFile)

	directiveDecl := "directive @idempotent on FIELD_DEFINITION"
	if !strings.Contains(schema, directiveDecl) {
		t.Errorf("schema missing %q\ngot:\n%s", directiveDecl, schema)
	}

	fieldWithDirective := "upsertBook(input: UpsertBookRequest!): UpsertBookResponse! @idempotent"
	if !strings.Contains(schema, fieldWithDirective) {
		t.Errorf("schema missing %q\ngot:\n%s", fieldWithDirective, schema)
	}
}

func TestBuildSchema_Golden(t *testing.T) {
	goldenFile := loadGoldenProtoFile(t)

	got := normalizeSchema(buildSchema(goldenFile))
	if testdataUpdateMode() {
		writeTestdata(t, "golden.schema.graphql", got)
		return
	}
	want := normalizeSchema(readTestdata(t, "golden.schema.graphql"))

	if got != want {
		gotLines := strings.Split(got, "\n")
		wantLines := strings.Split(want, "\n")
		maxLines := len(gotLines)
		if len(wantLines) > maxLines {
			maxLines = len(wantLines)
		}
		t.Errorf("buildSchema output does not match testdata/golden.schema.graphql\n")
		t.Logf("=== GOT ===\n%s", got)
		t.Logf("=== WANT ===\n%s", want)
		for i := 0; i < maxLines; i++ {
			g, w := "", ""
			if i < len(gotLines) {
				g = gotLines[i]
			}
			if i < len(wantLines) {
				w = wantLines[i]
			}
			if g != w {
				t.Logf("line %d diff:\n  got:  %q\n  want: %q", i+1, g, w)
			}
		}
	}
}
