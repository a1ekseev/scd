//go:build visual

package status

import (
	"bytes"
	"errors"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/chromedp/chromedp"
)

// visualGoldenDir holds the local reference screenshots of the dashboard
// blocks that have no TS reference. They are gitignored and never committed:
// -update creates them, and a test without its screenshot is skipped.
const visualGoldenDir = "testdata/visual/"

// warningVisible reports whether the first source warning of the local
// panel is rendered inside the viewport width.
const warningVisible = `(() => {
  const w = document.querySelector('#instance-panel-0 .metric-state-repairing');
  if (!w || w.getClientRects().length === 0) return false;
  const r = w.getBoundingClientRect();
  return r.width > 0 && r.left >= 0 && r.right <= window.innerWidth;
})()`

// The sources card against the Go reference PNGs (one per width), and its
// behaviour: the source warning is visible, switching the instance tab away
// and back hides and restores it and leaves the page as it was.
func TestDashboardSourcesVisual(t *testing.T) {
	bctx := startChrome(t)
	page := filepath.Join(t.TempDir(), "go.html")
	writeFile(t, page, []byte(dashboard(t, "sources-two", "/")))

	for _, w := range visualWidths {
		label := strconv.FormatInt(w, 10)
		t.Run(label, func(t *testing.T) {
			shot := screenshot(bctx, t, w, page, nil)
			matchGolden(t, filepath.Join(visualGoldenDir, "sources-two-"+label+".png"), shot)

			visible := func() bool {
				t.Helper()
				v, err := chromedp.Run(bctx, chromedp.Evaluate[bool](warningVisible))
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			if !visible() {
				t.Error("source warning is not visible")
			}
			if err := chromedp.Do(bctx, chromedp.Click("#instance-tab-1")); err != nil {
				t.Fatal(err)
			}
			if visible() {
				t.Error("source warning visible on the remote tab")
			}
			back := screenshot(bctx, t, w, page, []chromedp.Action[chromedp.Void]{
				chromedp.Click("#instance-tab-1"), chromedp.Click("#instance-tab-0"),
			})
			if !visible() {
				t.Error("source warning not restored on the local tab")
			}
			if ratio, _ := pixelDiff(decodePNG(t, shot), decodePNG(t, back)); ratio != 0 {
				t.Errorf("page after switching tabs back differs (ratio %.6f)", ratio)
			}
		})
	}
}

// The wireguard source with its counts and the no_replacement warning
// against the Go reference PNGs; the warning is visible at every width.
func TestDashboardWireGuardSourceVisual(t *testing.T) {
	bctx := startChrome(t)
	page := filepath.Join(t.TempDir(), "go.html")
	writeFile(t, page, []byte(dashboard(t, "sources-wireguard", "/")))
	for _, w := range visualWidths {
		label := strconv.FormatInt(w, 10)
		t.Run(label, func(t *testing.T) {
			matchGolden(t, filepath.Join(visualGoldenDir, "sources-wireguard-"+label+".png"), screenshot(bctx, t, w, page, nil))
			v, err := chromedp.Run(bctx, chromedp.Evaluate[bool](warningVisible))
			if err != nil {
				t.Fatal(err)
			}
			if !v {
				t.Error("no_replacement warning is not visible")
			}
		})
	}
}

// matchGolden compares the screenshot got with the PNG at path (written
// instead with -update): same size and at most maxDiffRatio differing
// pixels. A missing golden skips the test; on a mismatch the images are
// kept in a temporary directory.
func matchGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, got)
		return
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("no local golden %s; run with -update to create it", path)
	}
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	wantImg, gotImg := decodePNG(t, want), decodePNG(t, got)
	ratio, diff := pixelDiff(wantImg, gotImg)
	t.Logf("%s: want %v, got %v, diff ratio %.6f", path, wantImg.Bounds().Size(), gotImg.Bounds().Size(), ratio)
	if wantImg.Bounds().Size() == gotImg.Bounds().Size() && ratio <= maxDiffRatio {
		return
	}
	out, err := os.MkdirTemp("", "scd-visual-")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(out, "want.png"), want)
	writeFile(t, filepath.Join(out, "got.png"), got)
	var buf bytes.Buffer
	if err := png.Encode(&buf, diff); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(out, "diff.png"), buf.Bytes())
	t.Errorf("%s differs (ratio %.6f > %v, run with -update to regenerate); images in %s", path, ratio, maxDiffRatio, out)
}
