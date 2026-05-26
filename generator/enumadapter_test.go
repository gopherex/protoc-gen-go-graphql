package generator

import (
	"strings"
	"testing"
)

func TestBuildEnumAdapter_Golden(t *testing.T) {
	goldenFile := loadGoldenProtoFile(t)

	// The golden proto has exactly one enum: Genre.
	if len(goldenFile.Enums) == 0 {
		t.Fatal("golden.proto has no enums")
	}

	// These match the values generator.go derives from golden.proto's go_package.
	pbImport := "github.com/gopherex/protoc-gen-go-graphql/example/gen"

	got := normalizeSchema(buildEnumAdapter(goldenFile.Enums[0], pbImport))
	if testdataUpdateMode() {
		writeTestdata(t, "golden.genre.go.txt", got)
		return
	}
	want := normalizeSchema(readTestdata(t, "golden.genre.go.txt"))

	if got != want {
		gotLines := strings.Split(got, "\n")
		wantLines := strings.Split(want, "\n")
		maxLines := len(gotLines)
		if len(wantLines) > maxLines {
			maxLines = len(wantLines)
		}
		t.Errorf("buildEnumAdapter output does not match testdata/golden.genre.go.txt\n")
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
