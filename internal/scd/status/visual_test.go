//go:build visual

package status

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// maxDiffRatio is the largest share of differing pixels allowed between a
// screenshot and its golden PNG.
const maxDiffRatio = 0.001

// visualWidths are the viewport widths of the screenshots.
var visualWidths = []int64{1280, 390}

// ownVisualTest names the fixtures whose screenshots are checked by their
// own test, next to the behaviour they check.
var ownVisualTest = map[string]bool{"sources-two": true, "sources-wireguard": true, "wgp-ok": true, "wgp-needs-login": true}

// Every fixture's dashboard, rendered by headless Chrome (SCD_CHROME) at
// each width, against its local Go golden PNG (visualGoldenDir, -update).
// remote-instances is also shot after clicking the remote tab.
func TestDashboardVisual(t *testing.T) {
	bctx := startChrome(t)
	for _, name := range allFixtureNames(t) {
		if ownVisualTest[name] {
			continue
		}
		page := filepath.Join(t.TempDir(), "go.html")
		writeFile(t, page, []byte(dashboard(t, name, "/")))
		t.Run(name, func(t *testing.T) {
			for _, w := range visualWidths {
				label := strconv.FormatInt(w, 10)
				first := screenshot(bctx, t, w, page, nil)
				matchGolden(t, filepath.Join(visualGoldenDir, name+"-"+label+".png"), first)
				if name == "remote-instances" {
					second := screenshot(bctx, t, w, page, []chromedp.Action[chromedp.Void]{chromedp.Click("#instance-tab-1")})
					if bytes.Equal(first, second) {
						t.Errorf("%s: clicking #instance-tab-1 did not change the page", label)
					}
					matchGolden(t, filepath.Join(visualGoldenDir, name+"-"+label+"-tab-1.png"), second)
				}
			}
		})
	}
}

// startChrome starts the headless Chrome of SCD_CHROME (the test is skipped
// without it) and returns its browser context, closed with the test.
func startChrome(t *testing.T) context.Context {
	t.Helper()
	chrome := os.Getenv("SCD_CHROME")
	if chrome == "" {
		t.Skip("SCD_CHROME is not set")
	}
	actx, cancelAlloc := chromedp.NewExecAllocator(t.Context(),
		append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.NoSandbox)...)
	t.Cleanup(cancelAlloc)
	bctx, cancelBrowser := chromedp.NewContext(actx)
	t.Cleanup(cancelBrowser)
	if err := chromedp.Do(bctx); err != nil {
		t.Fatalf("start %s: %v", chrome, err)
	}
	return bctx
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// screenshot loads page at width w, runs steps and returns a full-page PNG.
func screenshot(ctx context.Context, t *testing.T, w int64, page string, steps []chromedp.Action[chromedp.Void]) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	actions := append([]chromedp.Action[chromedp.Void]{chromedp.EmulateViewport(w, 800), chromedp.Navigate("file://" + page)}, steps...)
	if err := chromedp.Do(ctx, actions...); err != nil {
		t.Fatalf("%s: %v", page, err)
	}
	data, err := chromedp.Run(ctx, chromedp.FullScreenshot(100))
	if err != nil {
		t.Fatalf("%s: screenshot: %v", page, err)
	}
	return data
}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// pixelDiff returns the share of differing pixels over the union of both
// images (a size mismatch counts the uncovered area as different) and an
// image marking them red.
func pixelDiff(a, b image.Image) (float64, *image.RGBA) {
	ab, bb := a.Bounds(), b.Bounds()
	width, height := max(ab.Dx(), bb.Dx()), max(ab.Dy(), bb.Dy())
	diff := image.NewRGBA(image.Rect(0, 0, width, height))
	red := color.RGBA{R: 255, A: 255}
	var n int
	for y := range height {
		for x := range width {
			pa, inA := pixelAt(a, x, y)
			pb, inB := pixelAt(b, x, y)
			if !inA || !inB || pa != pb {
				n++
				diff.Set(x, y, red)
				continue
			}
			g := color.GrayModel.Convert(pa).(color.Gray)
			diff.Set(x, y, color.RGBA{R: g.Y / 3, G: g.Y / 3, B: g.Y / 3, A: 255})
		}
	}
	return float64(n) / float64(width*height), diff
}

// pixelAt returns the RGBA of img at (x, y) relative to its origin.
func pixelAt(img image.Image, x, y int) (color.RGBA64, bool) {
	b := img.Bounds()
	p := image.Pt(b.Min.X+x, b.Min.Y+y)
	if !p.In(b) {
		return color.RGBA64{}, false
	}
	r, g, bl, al := img.At(p.X, p.Y).RGBA()
	return color.RGBA64{R: uint16(r), G: uint16(g), B: uint16(bl), A: uint16(al)}, true
}
