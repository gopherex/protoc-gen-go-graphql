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

// loadProtoWithOpts compiles inline protos that import graphqlopt/graphql.proto.
// It copies the repo's graphqlopt proto into the tmp tree and adds the WKT
// include dir so protoc resolves google/protobuf/descriptor.proto.
func loadProtoWithOpts(t *testing.T, files map[string]string, fileToGenerate []string) *protogen.Plugin {
	t.Helper()
	if _, err := exec.LookPath("protoc"); err != nil {
		t.Skip("protoc not found on PATH")
	}
	wkt := "/usr/include"
	if _, err := os.Stat(filepath.Join(wkt, "google/protobuf/descriptor.proto")); err != nil {
		t.Skip("WKT descriptor.proto not found at /usr/include")
	}
	optProto, err := os.ReadFile(filepath.Join("..", "graphqlopt", "graphql.proto"))
	if err != nil {
		t.Fatalf("read graphqlopt proto: %v", err)
	}

	tmp := t.TempDir()
	all := map[string]string{"graphqlopt/graphql.proto": string(optProto)}
	for k, v := range files {
		all[k] = v
	}
	for name, content := range all {
		p := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	desc := filepath.Join(tmp, "desc.pb")
	args := []string{"-I", tmp, "-I", wkt, "--include_imports", "--descriptor_set_out=" + desc}
	for _, name := range fileToGenerate {
		args = append(args, filepath.Join(tmp, filepath.FromSlash(name)))
	}
	if out, err := exec.Command("protoc", args...).CombinedOutput(); err != nil {
		t.Fatalf("protoc failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(desc)
	if err != nil {
		t.Fatalf("read desc: %v", err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		t.Fatalf("unmarshal desc: %v", err)
	}
	plugin, err := protogen.Options{}.New(&pluginpb.CodeGeneratorRequest{
		ProtoFile:      fds.File,
		FileToGenerate: fileToGenerate,
		Parameter:      proto.String("paths=source_relative"),
	})
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	return plugin
}

func b1Schema(t *testing.T, body string) string {
	t.Helper()
	src := `syntax = "proto3";
package b1.test;
option go_package = "example.test/gen/b1";
import "graphqlopt/graphql.proto";
` + body
	plugin := loadProtoWithOpts(t, map[string]string{"b1/b1.proto": src}, []string{"b1/b1.proto"})
	var file *protogen.File
	for _, f := range plugin.Files {
		if f.Desc.Path() == "b1/b1.proto" {
			file = f
		}
	}
	if file == nil {
		t.Fatal("b1/b1.proto not loaded")
	}
	schema, err := buildSchema(file)
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}
	return schema
}

func TestB1_MessageNameRename(t *testing.T) {
	schema := b1Schema(t, `
message GetReq { string id = 1; }
message Widget {
  option (graphqlopt.message) = { name: "RenamedWidget" };
  string label = 1;
}
message GetResp { Widget widget = 1; }
service API {
  rpc Get(GetReq) returns (GetResp) { option idempotency_level = NO_SIDE_EFFECTS; }
}`)
	if !strings.Contains(schema, "type RenamedWidget") {
		t.Fatalf("renamed type missing:\n%s", schema)
	}
	if strings.Contains(schema, "type Widget ") {
		t.Fatalf("original type name leaked:\n%s", schema)
	}
}

func TestB1_EnumNameRename(t *testing.T) {
	schema := b1Schema(t, `
enum Color {
  option (graphqlopt.enum) = { name: "Palette" };
  COLOR_UNSPECIFIED = 0;
  RED = 1;
}
message GetReq { string id = 1; }
message GetResp { Color color = 1; }
service API {
  rpc Get(GetReq) returns (GetResp) { option idempotency_level = NO_SIDE_EFFECTS; }
}`)
	if !strings.Contains(schema, "enum Palette") {
		t.Fatalf("renamed enum missing:\n%s", schema)
	}
	if !strings.Contains(schema, "color: Palette") {
		t.Fatalf("field does not reference renamed enum:\n%s", schema)
	}
}

func TestB1_FieldNameRenameAndExclude(t *testing.T) {
	schema := b1Schema(t, `
message GetReq { string id = 1; }
message GetResp {
  string keep_me = 1 [(graphqlopt.field) = { name: "renamedField" }];
  string drop_me = 2 [(graphqlopt.field) = { exclude: true }];
}
service API {
  rpc Get(GetReq) returns (GetResp) { option idempotency_level = NO_SIDE_EFFECTS; }
}`)
	if !strings.Contains(schema, "renamedField:") {
		t.Fatalf("renamed field missing:\n%s", schema)
	}
	if !strings.Contains(schema, `@goField(name: "KeepMe")`) {
		t.Fatalf("renamed field lacks @goField(name) mapping:\n%s", schema)
	}
	if strings.Contains(schema, "dropMe") {
		t.Fatalf("excluded field leaked:\n%s", schema)
	}
}

func TestB1_ServiceNamePrefix(t *testing.T) {
	schema := b1Schema(t, `
message GetReq { string id = 1; }
message GetResp { string id = 1; }
service API {
  option (graphqlopt.service) = { name_prefix: "admin" };
  rpc GetThing(GetReq) returns (GetResp) { option idempotency_level = NO_SIDE_EFFECTS; }
}`)
	if !strings.Contains(schema, "adminGetThing(") {
		t.Fatalf("service name_prefix not applied:\n%s", schema)
	}
}

func TestB1_OneofUnionNameOverride(t *testing.T) {
	schema := b1Schema(t, `
message Hit { string id = 1; }
message Miss { string reason = 1; }
message GetReq { string id = 1; }
message GetResp {
  oneof result {
    option (graphqlopt.oneof) = { union_name: "Lookup" };
    Hit hit = 1;
    Miss miss = 2;
  }
}
service API {
  rpc Get(GetReq) returns (GetResp) { option idempotency_level = NO_SIDE_EFFECTS; }
}`)
	if !strings.Contains(schema, "union Lookup") {
		t.Fatalf("union_name override not applied:\n%s", schema)
	}
}
