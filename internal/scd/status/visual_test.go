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

// maxDiffRatio is the largest share of differing pixels allowed between the
// TS and Go dashboards (D3).
const maxDiffRatio = 0.001

// visualWidths are the viewport widths of the task 0.4 screenshots.
var visualWidths = []int64{1280, 390}

// D3: the Go dashboard looks like the TS dashboard. For every 0.4 fixture
// the TS reference HTML and the Go HTML are rendered by the same headless
// Chrome (SCD_CHROME) at each width and compared pixel by pixel.
func TestDashboardVisualMatchesTS(t *testing.T) {
	bctx := startChrome(t)
	names := fixtureNames(t)
	pages := make(map[string][2]string, len(names))
	for _, name := range names {
		tsPage, goPage := writePages(t, name)
		pages[name] = [2]string{tsPage, goPage}
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			tsPage, goPage := pages[name][0], pages[name][1]

			for _, w := range visualWidths {
				label := strconv.FormatInt(w, 10)
				first := compareShots(bctx, t, name+"/"+label, w, tsPage, goPage, nil)
				if name == "remote-instances" {
					second := compareShots(bctx, t, name+"/"+label+"/tab-1", w, tsPage, goPage,
						[]chromedp.Action[chromedp.Void]{chromedp.Click("#instance-tab-1")})
					if bytes.Equal(first, second) {
						t.Errorf("%s: clicking #instance-tab-1 did not change the page", label)
					}
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

// writePages writes the TS reference HTML and the Go HTML of fixture name
// (served by the handler, frozen Now) to a temporary directory and returns
// their paths.
func writePages(t *testing.T, name string) (tsPage, goPage string) {
	t.Helper()
	dir := t.TempDir()
	tsPage, goPage = filepath.Join(dir, "ts.html"), filepath.Join(dir, "go.html")
	ref, err := os.ReadFile(refStatusDir + name + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, tsPage, ref)
	writeFile(t, goPage, []byte(dashboard(t, name, "/")))
	return tsPage, goPage
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// compareShots screenshots both pages at width w after steps and fails when
// the sizes differ or more than maxDiffRatio of the pixels differ. It
// returns the Go screenshot.
func compareShots(ctx context.Context, t *testing.T, label string, w int64, tsPage, goPage string, steps []chromedp.Action[chromedp.Void]) []byte {
	t.Helper()
	tsPNG := screenshot(ctx, t, w, tsPage, steps)
	goPNG := screenshot(ctx, t, w, goPage, steps)
	tsImg, goImg := decodePNG(t, tsPNG), decodePNG(t, goPNG)

	ratio, diff := pixelDiff(tsImg, goImg)
	t.Logf("%s: ts %v, go %v, diff ratio %.6f", label, tsImg.Bounds().Size(), goImg.Bounds().Size(), ratio)
	if tsImg.Bounds().Size() == goImg.Bounds().Size() && ratio <= maxDiffRatio {
		return goPNG
	}
	out, err := os.MkdirTemp("", "scd-visual-")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(out, "ts.png"), tsPNG)
	writeFile(t, filepath.Join(out, "go.png"), goPNG)
	var buf bytes.Buffer
	if err := png.Encode(&buf, diff); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(out, "diff.png"), buf.Bytes())
	t.Errorf("%s: dashboards differ (ts %v, go %v, ratio %.6f > %v); images in %s",
		label, tsImg.Bounds().Size(), goImg.Bounds().Size(), ratio, maxDiffRatio, out)
	return goPNG
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
