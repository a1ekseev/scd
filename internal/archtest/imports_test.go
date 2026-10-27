package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePrefix = "github.com/a1ekseev/scd/internal/"

var services = []string{"scd", "sfc", "gtl"}

// importsOf returns every import path of every .go file under dir (relative
// to internal/), keyed by file path. It fails when dir holds no .go file, so
// a moved or renamed service cannot make the checks pass vacuously.
func importsOf(t *testing.T, dir string) map[string][]string {
	t.Helper()
	result := map[string][]string{}
	files := 0
	err := filepath.WalkDir(filepath.Join("..", dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			result[path] = append(result[path], p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if files == 0 {
		t.Fatalf("no .go files under internal/%s", dir)
	}
	return result
}

func TestServicesDoNotImportEachOther(t *testing.T) {
	for _, svc := range services {
		for file, imports := range importsOf(t, svc) {
			for _, other := range services {
				if other == svc {
					continue
				}
				forbidden := modulePrefix + other
				for _, p := range imports {
					if p == forbidden || strings.HasPrefix(p, forbidden+"/") {
						t.Errorf("%s imports %s", file, p)
					}
				}
			}
		}
	}
}

func TestCommonDoesNotImportServices(t *testing.T) {
	for file, imports := range importsOf(t, "common") {
		for _, svc := range services {
			forbidden := modulePrefix + svc
			for _, p := range imports {
				if p == forbidden || strings.HasPrefix(p, forbidden+"/") {
					t.Errorf("%s imports %s", file, p)
				}
			}
		}
	}
}
