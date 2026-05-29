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

// loadProtoFile compiles a single inline .proto (written into a temp dir under
// the repo so the graphqlopt import resolves) and returns its *protogen.File.
// The test is skipped if protoc is not on PATH.
func loadProtoFile(t *testing.T, src string) *protogen.File {
	t.Helper()
	if _, err := exec.LookPath("protoc"); err != nil {
		t.Skip("protoc not found on PATH, skipping")
	}
	root := repoRoot(t)

	dir, err := os.MkdirTemp(root, "opttest-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)

	protoPath := filepath.Join(dir, "t.proto")
	if err := os.WriteFile(protoPath, []byte(src), 0644); err != nil {
		t.Fatalf("write proto: %v", err)
	}

	setOut := filepath.Join(dir, "t.pb")
	cmd := exec.Command("protoc",
		"-I", dir,
		"-I", root,
		"-I", "/usr/include",
		"--include_imports",
		"--descriptor_set_out="+setOut,
		protoPath,
	)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("protoc failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(setOut)
	if err != nil {
		t.Fatalf("read descriptor set: %v", err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	req := &pluginpb.CodeGeneratorRequest{
		ProtoFile:      fds.File,
		FileToGenerate: []string{"t.proto"},
		Parameter:      proto.String("paths=source_relative"),
	}
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	for _, f := range plugin.Files {
		if f.Desc.Path() == "t.proto" {
			return f
		}
	}
	t.Fatal("t.proto not found")
	return nil
}

// TestSkipOmitsMethodAndOverrideApplies uses the golden proto (which marks
// ShipLogs skip and renames GetScalars via operation_name) to assert that the
// skipped client-streaming rpc is gone and the override field name is present.
func TestSkipOmitsMethodAndOverrideApplies(t *testing.T) {
	f := loadGoldenProtoFile(t)
	schema, err := buildSchema(f)
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}
	for _, bad := range []string{"ShipLogs", "shipLogs", "LogChunk", "ShipAck"} {
		if strings.Contains(schema, bad) {
			t.Errorf("skipped rpc/message %q leaked into schema:\n%s", bad, schema)
		}
	}
	if !strings.Contains(schema, "fetchScalars(input: GetScalarsRequest!): GetScalarsResponse!") {
		t.Errorf("operation_name override 'fetchScalars' missing from schema:\n%s", schema)
	}
	if strings.Contains(schema, "getScalars(") {
		t.Errorf("original field name 'getScalars' should be replaced by override:\n%s", schema)
	}

	// The skipped client-streaming rpc must NOT cause a fail-fast through the
	// full generateFiles path either (graph build + validations).
	if g := graphFromFile(f); g == nil {
		t.Fatal("graphFromFile returned nil")
	}
}

// TestOperationOverrideMovesRoot asserts a forced operation override changes the
// root a method lands under (a NO_SIDE_EFFECTS query forced to MUTATION).
func TestOperationOverrideMovesRoot(t *testing.T) {
	src := `syntax = "proto3";
package t.v1;
option go_package = "example.com/t;t";
import "graphqlopt/graphql.proto";
service S {
  rpc Q(Req) returns (Resp) {
    option idempotency_level = NO_SIDE_EFFECTS;
    option (graphqlopt.method) = { operation: MUTATION };
  }
}
message Req { string id = 1; }
message Resp { string out = 1; }
`
	f := loadProtoFile(t, src)
	schema, err := buildSchema(f)
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}
	if strings.Contains(schema, "type Query") {
		t.Errorf("query forced to MUTATION should not appear under Query:\n%s", schema)
	}
	if !strings.Contains(schema, "type Mutation") || !strings.Contains(schema, "q(input: Req!): Resp!") {
		t.Errorf("forced MUTATION field missing:\n%s", schema)
	}
}

// TestUnsupportedOptionErrors asserts that a set-but-unimplemented option
// (FieldOptions.exclude here) produces a clear "not yet implemented" error.
func TestUnsupportedOptionErrors(t *testing.T) {
	src := `syntax = "proto3";
package t.v1;
option go_package = "example.com/t;t";
import "graphqlopt/graphql.proto";
service S {
  rpc Q(Req) returns (Resp) { option idempotency_level = NO_SIDE_EFFECTS; }
}
message Req { string id = 1 [(graphqlopt.field) = { scalar: "MyScalar" }]; }
message Resp { string out = 1; }
`
	f := loadProtoFile(t, src)
	g := graphFromFile(f)
	err := validateUnsupportedOptions(g)
	if err == nil {
		t.Fatal("expected error for set-but-unimplemented FieldOptions.scalar, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") || !strings.Contains(err.Error(), "FieldOptions.scalar") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestOperationOverrideConflictErrors asserts a unary rpc forced to SUBSCRIPTION
// fails fast.
func TestOperationOverrideConflictErrors(t *testing.T) {
	src := `syntax = "proto3";
package t.v1;
option go_package = "example.com/t;t";
import "graphqlopt/graphql.proto";
service S {
  rpc Q(Req) returns (Resp) {
    option (graphqlopt.method) = { operation: SUBSCRIPTION };
  }
}
message Req { string id = 1; }
message Resp { string out = 1; }
`
	f := loadProtoFile(t, src)
	g := graphFromFile(f)
	err := validateOperationOverrides(g)
	if err == nil || !strings.Contains(err.Error(), "SUBSCRIPTION") {
		t.Fatalf("expected unary->SUBSCRIPTION conflict error, got %v", err)
	}
}

// TestSkippedMessageReferencedByRpcErrors asserts referencing a skipped message
// from a non-skipped rpc fails fast.
func TestSkippedMessageReferencedByRpcErrors(t *testing.T) {
	src := `syntax = "proto3";
package t.v1;
option go_package = "example.com/t;t";
import "graphqlopt/graphql.proto";
service S {
  rpc Q(Req) returns (Resp) { option idempotency_level = NO_SIDE_EFFECTS; }
}
message Req { string id = 1; }
message Resp { Hidden h = 1; }
message Hidden {
  option (graphqlopt.message) = { skip: true };
  string secret = 1;
}
`
	f := loadProtoFile(t, src)
	err := validateSkippedReferences([]*protogen.File{f})
	if err == nil || !strings.Contains(err.Error(), "skipped") {
		t.Fatalf("expected skipped-message-referenced error, got %v", err)
	}
}
