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

// TestBuildSchema_EmptyMessages asserts that fieldless proto messages are handled
// correctly: empty request → no input arg; empty output → ok placeholder field.
func TestBuildSchema_EmptyMessages(t *testing.T) {
	goldenFile := loadGoldenProtoFile(t)
	schema, err := buildSchema(goldenFile)
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}

	// Empty request (PingRequest): operation field has NO input arg.
	noArgField := "ping: PingResponse!"
	if !strings.Contains(schema, noArgField) {
		t.Errorf("schema missing no-arg Ping field %q\ngot:\n%s", noArgField, schema)
	}
	// Ensure the old broken form is absent.
	badPing := "ping(input:"
	if strings.Contains(schema, badPing) {
		t.Errorf("schema should not have %q for empty request\ngot:\n%s", badPing, schema)
	}

	// Empty output (PingResponse): must have ok placeholder with forceResolver.
	okField := "type PingResponse { ok: Boolean! @goField(forceResolver: true) }"
	if !strings.Contains(schema, okField) {
		t.Errorf("schema missing empty-output placeholder %q\ngot:\n%s", okField, schema)
	}
	// Ensure the old broken blank-name form is absent.
	if strings.Contains(schema, "{ _: Boolean }") {
		t.Errorf("schema must not contain blank-field placeholder '{ _: Boolean }'\ngot:\n%s", schema)
	}

	// PingRequest must NOT appear as an input type.
	if strings.Contains(schema, "input PingRequest") {
		t.Errorf("schema must not emit an input type for empty PingRequest\ngot:\n%s", schema)
	}

	// Empty NESTED input (Container.Settings, used by EchoRequest.settings): must
	// emit a placeholder input object with a forceResolver field (not fail-fast).
	emptyNestedInput := "input Container_SettingsInput { _empty: Boolean @goField(forceResolver: true) }"
	if !strings.Contains(schema, emptyNestedInput) {
		t.Errorf("schema missing empty nested input placeholder %q\ngot:\n%s", emptyNestedInput, schema)
	}
}

// TestBuildResolvers_EmptyNestedInput asserts that an empty nested input message
// gets a no-op placeholder resolver satisfying the gqlgen input resolver interface.
func TestBuildResolvers_EmptyNestedInput(t *testing.T) {
	goldenFile := loadGoldenProtoFile(t)
	pbImport := "github.com/gopherex/protoc-gen-go-graphql/example/gen"
	resolvers := buildResolvers(goldenFile, "gqlapi",
		pbImport, pbImport+"/gqlapi/pbgql",
		pbImport+"/gqlapi/exec", "github.com/gopherex/protoc-gen-go-graphql/graphqlpb")

	for _, want := range []string{
		"func (r *Resolver) Container_SettingsInput() exec.Container_SettingsInputResolver",
		"type container_SettingsInputResolver struct{ *Resolver }",
		"func (r container_SettingsInputResolver) Empty(ctx context.Context, obj *pb.Container_Settings, data *bool) error {",
	} {
		if !strings.Contains(resolvers, want) {
			t.Errorf("resolvers missing %q\ngot:\n%s", want, resolvers)
		}
	}
}

// TestBuildSchema_IdempotentDirective asserts that the schema contains the
// @idempotent directive declaration and that the upsertBook mutation field
// carries the @idempotent annotation.
func TestBuildSchema_IdempotentDirective(t *testing.T) {
	goldenFile := loadGoldenProtoFile(t)
	schema, err := buildSchema(goldenFile)
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}

	directiveDecl := "directive @idempotent on FIELD_DEFINITION"
	if !strings.Contains(schema, directiveDecl) {
		t.Errorf("schema missing %q\ngot:\n%s", directiveDecl, schema)
	}

	fieldWithDirective := "upsertBook(input: UpsertBookRequest!): UpsertBookResponse! @idempotent"
	if !strings.Contains(schema, fieldWithDirective) {
		t.Errorf("schema missing %q\ngot:\n%s", fieldWithDirective, schema)
	}
}

// buildAllOneofAdapters builds the pbgql oneof adapter source for every message
// in g that contains oneofs, concatenated. Used by the input_mode tests to
// inspect the emitted ToPb shims without driving the full generate pipeline.
func buildAllOneofAdapters(g *graph) string {
	msgInfo := analyzeMessagesGraph(g)
	ois := collectOneofsGraph(g, msgInfo)
	byMsg := map[string][]oneofInfo{}
	for _, oi := range ois {
		byMsg[messageKey(oi.Msg)] = append(byMsg[messageKey(oi.Msg)], oi)
	}
	var out strings.Builder
	for _, msg := range g.Messages {
		if mo := byMsg[messageKey(msg)]; len(mo) > 0 {
			out.WriteString(buildOneofAdapter(mo, string(msg.GoIdent.GoImportPath)))
		}
	}
	return out.String()
}

// TestOneofInputMode_AllNullable asserts that an ALL_NULLABLE input oneof emits
// a plain input object (no @oneOf) plus a ToPb shim with a runtime exactly-one
// check, while UNSPECIFIED and DIRECTIVE modes both emit a schema @oneOf input
// with a ToPb shim that has no runtime count check.
func TestOneofInputMode_AllNullable(t *testing.T) {
	src := `syntax = "proto3";
package t.v1;
option go_package = "example.com/t;t";
import "graphqlopt/graphql.proto";
service S {
  rpc Default(DefaultReq) returns (Resp) { option idempotency_level = NO_SIDE_EFFECTS; }
  rpc Directive(DirectiveReq) returns (Resp) { option idempotency_level = NO_SIDE_EFFECTS; }
  rpc Nullable(NullableReq) returns (Resp) { option idempotency_level = NO_SIDE_EFFECTS; }
}
message DefaultReq {
  oneof key { string a = 1; string b = 2; }
}
message DirectiveReq {
  oneof key {
    option (graphqlopt.oneof) = { input_mode: ONEOF_DIRECTIVE };
    string a = 1;
    string b = 2;
  }
}
message NullableReq {
  oneof key {
    option (graphqlopt.oneof) = { input_mode: ALL_NULLABLE };
    string a = 1;
    string b = 2;
  }
}
message Resp { string out = 1; }
`
	f := loadProtoFile(t, src)
	g := graphFromFile(f)

	schema, err := buildSchema(f)
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}

	// DIRECTIVE + UNSPECIFIED → @oneOf; ALL_NULLABLE → plain input.
	if !strings.Contains(schema, "input DefaultReqKey @oneOf {") {
		t.Errorf("UNSPECIFIED mode should emit @oneOf input; got:\n%s", schema)
	}
	if !strings.Contains(schema, "input DirectiveReqKey @oneOf {") {
		t.Errorf("DIRECTIVE mode should emit @oneOf input; got:\n%s", schema)
	}
	if !strings.Contains(schema, "input NullableReqKey {") {
		t.Errorf("ALL_NULLABLE mode should emit a plain input; got:\n%s", schema)
	}
	if strings.Contains(schema, "input NullableReqKey @oneOf") {
		t.Errorf("ALL_NULLABLE mode must NOT carry @oneOf; got:\n%s", schema)
	}

	// ToPb shims: signature returns (*pb.X, error) in all modes; only
	// ALL_NULLABLE has the runtime exactly-one count check.
	adapters := buildAllOneofAdapters(g)
	for _, sig := range []string{
		"func ToPbDefaultReq(r *DefaultReqInput) (*pb.DefaultReq, error) {",
		"func ToPbDirectiveReq(r *DirectiveReqInput) (*pb.DirectiveReq, error) {",
		"func ToPbNullableReq(r *NullableReqInput) (*pb.NullableReq, error) {",
	} {
		if !strings.Contains(adapters, sig) {
			t.Errorf("missing ToPb signature %q\ngot:\n%s", sig, adapters)
		}
	}
	// The ALL_NULLABLE shim must enforce exactly-one at runtime.
	if !strings.Contains(adapters, "if set != 1 {") ||
		!strings.Contains(adapters, "must be set for NullableReqKey") {
		t.Errorf("ALL_NULLABLE ToPb missing runtime exactly-one check\ngot:\n%s", adapters)
	}
	// DIRECTIVE/UNSPECIFIED shims must NOT carry the runtime count check.
	for _, msg := range []string{"DefaultReq", "DirectiveReq"} {
		fn := "func ToPb" + msg + "("
		idx := strings.Index(adapters, fn)
		end := strings.Index(adapters[idx:], "\n}\n")
		body := adapters[idx : idx+end]
		if strings.Contains(body, "if set != 1") {
			t.Errorf("%s mode ToPb should not have a runtime count check\nbody:\n%s", msg, body)
		}
	}
}

func TestBuildSchema_Golden(t *testing.T) {
	goldenFile := loadGoldenProtoFile(t)

	schema, err := buildSchema(goldenFile)
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}
	got := normalizeSchema(schema)
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
