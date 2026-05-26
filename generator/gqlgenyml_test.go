package generator

import (
	"strings"
	"testing"
)

func TestBuildGqlgenYml_Golden(t *testing.T) {
	goldenFile := loadGoldenProtoFile(t)

	// These match the values generator.go derives from golden.proto's go_package.
	pbImport := "github.com/gopherex/protoc-gen-go-graphql/example/gen"
	pbgqlImport := "github.com/gopherex/protoc-gen-go-graphql/example/gen/gqlapi/pbgql"

	got := normalizeSchema(buildGqlgenYml(goldenFile, pbImport, pbgqlImport))
	if testdataUpdateMode() {
		writeTestdata(t, "golden.gqlgen.yml", got)
		return
	}
	want := normalizeSchema(readTestdata(t, "golden.gqlgen.yml"))

	if got != want {
		gotLines := strings.Split(got, "\n")
		wantLines := strings.Split(want, "\n")
		maxLines := len(gotLines)
		if len(wantLines) > maxLines {
			maxLines = len(wantLines)
		}
		t.Errorf("buildGqlgenYml output does not match testdata/golden.gqlgen.yml\n")
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
