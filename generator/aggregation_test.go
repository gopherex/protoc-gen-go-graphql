package generator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

func loadProtoPlugin(t *testing.T, files map[string]string, fileToGenerate []string) *protogen.Plugin {
	t.Helper()
	if _, err := exec.LookPath("protoc"); err != nil {
		t.Skip("protoc not found on PATH")
	}

	tmp := t.TempDir()
	for name, content := range files {
		path := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	desc := filepath.Join(tmp, "desc.pb")
	args := []string{"-I", tmp, "--include_imports", "--descriptor_set_out=" + desc}
	for _, name := range fileToGenerate {
		args = append(args, filepath.Join(tmp, filepath.FromSlash(name)))
	}
	cmd := exec.Command("protoc", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("protoc failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(desc)
	if err != nil {
		t.Fatalf("read descriptor set: %v", err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		t.Fatalf("unmarshal descriptor set: %v", err)
	}

	req := &pluginpb.CodeGeneratorRequest{
		ProtoFile:      fds.File,
		FileToGenerate: fileToGenerate,
		Parameter:      proto.String("paths=source_relative"),
	}
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	return plugin
}

func TestGenerateAggregatesFilesByGoPackage(t *testing.T) {
	plugin := loadProtoPlugin(t, map[string]string{
		"api/a.proto": `syntax = "proto3";
package test.api;
option go_package = "example.test/gen/api";
message GetARequest {}
message GetAResponse { string id = 1; }
service AlphaAPI {
  rpc GetA(GetARequest) returns (GetAResponse) { option idempotency_level = NO_SIDE_EFFECTS; }
}`,
		"api/b.proto": `syntax = "proto3";
package test.api;
option go_package = "example.test/gen/api";
message GetBRequest {}
message GetBResponse { string id = 1; }
service BetaAPI {
  rpc GetB(GetBRequest) returns (GetBResponse) { option idempotency_level = NO_SIDE_EFFECTS; }
}`,
	}, []string{"api/a.proto", "api/b.proto"})

	if err := New(plugin, &Settings{Paths: "source_relative"}).Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	resp := plugin.Response()
	counts := map[string]int{}
	var schema string
	for _, f := range resp.File {
		counts[f.GetName()]++
		if f.GetName() == "api/gqlapi/schema.go" {
			schema = f.GetContent()
		}
	}
	for name, count := range counts {
		if count > 1 {
			t.Fatalf("duplicate generated file %s count=%d", name, count)
		}
	}
	if counts["api/gqlapi/schema.go"] != 1 {
		t.Fatalf("expected one aggregated gqlapi schema.go, got counts: %#v", counts)
	}
	// Both services from the same Go package aggregate into one schema.go, each
	// RPC becoming a Query field on the graphql-go schema.
	if !strings.Contains(schema, `"getA"`) || !strings.Contains(schema, `"getB"`) {
		t.Fatalf("schema does not include both services:\n%s", schema)
	}
}

func TestSchemaQualifiesCrossPackageRPCMessages(t *testing.T) {
	plugin := loadProtoPlugin(t, map[string]string{
		"common/common.proto": `syntax = "proto3";
package test.common;
option go_package = "example.test/gen/common";
message ExternalRequest { string id = 1; }
message ExternalResponse { string id = 1; }
`,
		"api/api.proto": `syntax = "proto3";
package test.api;
option go_package = "example.test/gen/api";
import "common/common.proto";
service CrossAPI {
  rpc Fetch(test.common.ExternalRequest) returns (test.common.ExternalResponse) { option idempotency_level = NO_SIDE_EFFECTS; }
}`,
	}, []string{"api/api.proto"})

	var apiFile *protogen.File
	for _, f := range plugin.Files {
		if f.Desc.Path() == "api/api.proto" {
			apiFile = f
			break
		}
	}
	if apiFile == nil {
		t.Fatal("api/api.proto not found")
	}

	schema, err := buildGraphQLGoGraph(
		graphFromFiles([]*protogen.File{apiFile}),
		"gqlapi",
		"example.test/gen/api",
		"github.com/gopherex/protoc-gen-go-graphql/graphqlrt",
	)
	if err != nil {
		t.Fatalf("buildGraphQLGoGraph: %v", err)
	}
	// The cross-package response message gets its own aliased pb import and is
	// qualified by it, not by the api package's "pb" alias.
	if !strings.Contains(schema, `pb1 "example.test/gen/common"`) {
		t.Fatalf("schema missing external pb import:\n%s", schema)
	}
	if !strings.Contains(schema, "*pb1.ExternalResponse") {
		t.Fatalf("schema did not qualify cross-package RPC type:\n%s", schema)
	}
}

func TestSchemaEmptyMessages(t *testing.T) {
	plugin := loadProtoPlugin(t, map[string]string{
		"api/empty.proto": `syntax = "proto3";
package test.api;
option go_package = "example.test/gen/api";
message EmptyRequest {}
message EmptyResponse {}
service EmptyAPI {
  rpc Get(EmptyRequest) returns (EmptyResponse) { option idempotency_level = NO_SIDE_EFFECTS; }
}`,
	}, []string{"api/empty.proto"})

	var file *protogen.File
	for _, f := range plugin.Files {
		if f.Desc.Path() == "api/empty.proto" {
			file = f
			break
		}
	}
	if file == nil {
		t.Fatal("api/empty.proto not found")
	}

	schema, err := buildGraphQLGoGraph(graphFromFile(file), "gqlapi", "example.test/gen/api",
		"github.com/gopherex/protoc-gen-go-graphql/graphqlrt")
	if err != nil {
		t.Fatalf("buildGraphQLGoGraph: %v", err)
	}
	// Empty request: the operation constructs the request inline (no decode/args).
	if !strings.Contains(schema, "&pb.EmptyRequest{}") {
		t.Fatalf("empty request should be constructed inline:\n%s", schema)
	}
	// Empty output: placeholder `ok` field returning true.
	if !strings.Contains(schema, `"ok"`) {
		t.Fatalf("empty output should emit an ok placeholder field:\n%s", schema)
	}
}
