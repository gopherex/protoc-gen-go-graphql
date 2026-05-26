package generator

// Integration test for buildSchema. Rather than unit-constructing a
// *protogen.File (which requires driving protoc's wire protocol), this test
// shells out to protoc to compile golden.proto into a FileDescriptorSet, loads
// it via protodesc + protogen, calls buildSchema, and compares the result to
// spike/schema.graphql (normalized: trailing whitespace stripped, trailing
// newlines collapsed to one).
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

func TestBuildSchema_Golden(t *testing.T) {
	// Skip if protoc is not available.
	if _, err := exec.LookPath("protoc"); err != nil {
		t.Skip("protoc not found on PATH, skipping schema golden test")
	}

	root := repoRoot(t)
	exampleDir := filepath.Join(root, "example")
	wktInc := "/usr/include"

	// Compile golden.proto to a descriptor set in a temp file.
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

	// Load the descriptor set.
	raw, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read descriptor set: %v", err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		t.Fatalf("unmarshal FileDescriptorSet: %v", err)
	}

	// Find the golden.proto file index.
	goldenIdx := -1
	var fileToGen []string
	for i, fd := range fds.File {
		if fd.GetName() == "golden.proto" {
			goldenIdx = i
			fileToGen = append(fileToGen, fd.GetName())
		}
	}
	if goldenIdx < 0 {
		t.Fatal("golden.proto not found in descriptor set")
	}

	// Build a CodeGeneratorRequest.
	req := &pluginpb.CodeGeneratorRequest{
		ProtoFile:      fds.File,
		FileToGenerate: fileToGen,
		Parameter:      proto.String("paths=source_relative"),
	}

	// Create a Plugin from the request.
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}

	// Find the golden file.
	var goldenFile *protogen.File
	for _, f := range plugin.Files {
		if f.Desc.Path() == "golden.proto" {
			goldenFile = f
			break
		}
	}
	if goldenFile == nil {
		t.Fatal("golden.proto not found in plugin.Files")
	}

	// Call buildSchema.
	got := normalizeSchema(buildSchema(goldenFile))

	// Read the spike schema.
	spikeSchema, err := os.ReadFile(filepath.Join(root, "spike", "schema.graphql"))
	if err != nil {
		t.Fatalf("read spike schema: %v", err)
	}
	want := normalizeSchema(string(spikeSchema))

	if got != want {
		// Print a diff-friendly view.
		gotLines := strings.Split(got, "\n")
		wantLines := strings.Split(want, "\n")
		maxLines := len(gotLines)
		if len(wantLines) > maxLines {
			maxLines = len(wantLines)
		}
		t.Errorf("buildSchema output does not match spike/schema.graphql\n")
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
