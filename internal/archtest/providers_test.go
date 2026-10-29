package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// providerName matches a candidate provider name anywhere, any case.
var providerName = regexp.MustCompile(`(?i)proton|warp`)

// xrayNames are the Xray config and proto names that contain a provider
// name without naming the provider: the device block of masqueSettings.
var xrayNames = regexp.MustCompile(`json:"warp"|\.GetWarp\(\)`)

// scd never branches on a provider: provider names live only in the
// provider table of common/candidate. Test files and test data may name
// them.
func TestNoProviderNamesInSCD(t *testing.T) {
	files := 0
	err := filepath.WalkDir(filepath.Join("..", "scd"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		files++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if m := providerName.Find(xrayNames.ReplaceAll(b, nil)); m != nil {
			t.Errorf("%s names the provider %q", path, m)
		}
		return nil
	})
	if err != nil || files == 0 {
		t.Fatalf("walk: %v, %d files", err, files)
	}
}
