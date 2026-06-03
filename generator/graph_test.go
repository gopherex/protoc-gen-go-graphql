package generator

import "testing"

// TestGraphHasOperations covers the "skip packages with no enabled services"
// behavior: a message-only proto and a proto whose every method is skipped both
// report no operations (so generateFiles emits no gqlapi for them), while a proto
// with at least one non-skipped method reports operations.
func TestGraphHasOperations(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "message only (no services)",
			src: `syntax = "proto3";
package t.v1;
option go_package = "example.com/t;t";
message Foo { string a = 1; }`,
			want: false,
		},
		{
			name: "all methods skipped",
			src: `syntax = "proto3";
package t.v1;
option go_package = "example.com/t;t";
import "graphqlopt/graphql.proto";
service S {
  rpc M(Foo) returns (Foo) {
    option idempotency_level = NO_SIDE_EFFECTS;
    option (graphqlopt.method) = { skip: true };
  }
}
message Foo { string a = 1; }`,
			want: false,
		},
		{
			name: "has a method",
			src: `syntax = "proto3";
package t.v1;
option go_package = "example.com/t;t";
service S {
  rpc M(Foo) returns (Foo) { option idempotency_level = NO_SIDE_EFFECTS; }
}
message Foo { string a = 1; }`,
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := loadProtoFile(t, tc.src)
			if got := graphHasOperations(graphFromFile(f)); got != tc.want {
				t.Errorf("graphHasOperations = %v, want %v", got, tc.want)
			}
		})
	}
}
