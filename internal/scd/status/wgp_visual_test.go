//go:build visual

package status

import (
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/chromedp/chromedp"
)

// wgpCardsJS returns, for each wgp card of the local panel that is rendered
// inside the viewport width, its title and whether its orb has the degraded
// colour (var(--degraded)).
const wgpCardsJS = `(() => {
  const panel = document.querySelector('#instance-panel-0');
  const h = Array.from(panel.querySelectorAll('h2')).find((e) => e.textContent === 'wgp');
  if (!h) return [];
  const degraded = getComputedStyle(document.documentElement).getPropertyValue('--degraded').trim();
  const probe = document.createElement('div');
  probe.style.background = degraded;
  document.body.appendChild(probe);
  const want = getComputedStyle(probe).backgroundColor;
  probe.remove();
  return Array.from(h.closest('section').querySelectorAll('.balancer-card')).filter((c) => {
    const r = c.getBoundingClientRect();
    return c.getClientRects().length > 0 && r.width > 0 && r.left >= 0 && r.right <= window.innerWidth;
  }).map((c) => c.querySelector('.node-title').textContent + '|' +
    (getComputedStyle(c.querySelector('.node-orb')).backgroundColor === want));
})()`

// The wgp cards against the Go reference PNGs (one per fixture and width)
// and their behaviour: every card is visible, exactly the degraded ones have
// the degraded orb, and the remote tab hides them.
func TestDashboardWGPVisual(t *testing.T) {
	bctx := startChrome(t)
	want := map[string][]string{
		"wgp-ok":          {"acc · wg|false", "dev · mq|false"},
		"wgp-needs-login": {"sub-1/mq|true", "acc · wg|true", "soon · wg2|true"},
	}
	for _, name := range []string{"wgp-ok", "wgp-needs-login"} {
		page := filepath.Join(t.TempDir(), "go.html")
		writeFile(t, page, []byte(dashboard(t, name, "/")))
		for _, w := range visualWidths {
			label := strconv.FormatInt(w, 10)
			t.Run(name+"/"+label, func(t *testing.T) {
				matchGolden(t, filepath.Join(visualGoldenDir, name+"-"+label+".png"), screenshot(bctx, t, w, page, nil))
				cards := func() []string {
					t.Helper()
					v, err := chromedp.Run(bctx, chromedp.Evaluate[[]string](wgpCardsJS))
					if err != nil {
						t.Fatal(err)
					}
					return v
				}
				if got := cards(); !slices.Equal(got, want[name]) {
					t.Errorf("wgp cards %q, want %q", got, want[name])
				}
				if name != "wgp-ok" {
					return
				}
				if err := chromedp.Do(bctx, chromedp.Click("#instance-tab-1")); err != nil {
					t.Fatal(err)
				}
				if got := cards(); len(got) != 0 {
					t.Errorf("wgp cards visible on the remote tab: %q", got)
				}
			})
		}
	}
}
