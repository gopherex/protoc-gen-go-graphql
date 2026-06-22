package generator

// Shared test helpers: protoc-driven loading of golden.proto into a
// *protogen.File, repo-root resolution, and testdata read/write.

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
	return filepath.Dir(filepath.Dir(file))
}

func normalizeSchema(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		out = append(out, strings.TrimRight(l, " \t"))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
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

	tmp, err := os.CreateTemp("", "golden-*.pb")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	cmd := exec.Command("protoc",
		"-I", exampleDir,
		"-I", root,
		"-I", "/usr/include",
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

	plugin, err := protogen.Options{}.New(&pluginpb.CodeGeneratorRequest{
		ProtoFile:      fds.File,
		FileToGenerate: fileToGen,
		Parameter:      proto.String("paths=source_relative"),
	})
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
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "generator", "testdata", name))
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return string(data)
}

// writeTestdata writes a file to generator/testdata/ (used when TESTDATA_UPDATE=1).
func writeTestdata(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(repoRoot(t), "generator", "testdata", name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write testdata %s: %v", name, err)
	}
	t.Logf("updated testdata/%s", name)
}

// testdataUpdateMode returns true when TESTDATA_UPDATE=1 is set.
func testdataUpdateMode() bool { return os.Getenv("TESTDATA_UPDATE") == "1" }
