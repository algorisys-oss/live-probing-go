package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"liveprobe/internal/core"
)

// Port of e2e/live.spec.ts, plus the Go UI's live-streaming behaviours.

func TestLiveFeedListsSeededTraces(t *testing.T) {
	b := open(t)
	b.goTo("/")
	b.waitJS("trace list", visibleCount(".trace-list")+" > 0")
	for _, op := range []string{ops.checkout, ops.cart, ops.catalog} {
		b.waitJS("row "+op, withText(".trace-row", op))
	}
}

func TestFlowViewRendersSeededServices(t *testing.T) {
	b := open(t)
	b.goTo("/")
	b.waitJS("flow svg", visibleCount(".flow-svg")+" > 0")
	if b.jsInt(count(".flow-node-group")) == 0 {
		t.Fatal("no flow nodes")
	}
	b.waitJS("gateway label", textOf(".flow-svg")+".includes('"+gateway+"')")
}

func TestUninstrumentedPeerIsAGhostNode(t *testing.T) {
	b := open(t)
	b.goTo("/")
	b.waitJS("one ghost node", count(".flow-node-external")+" === 1")
	b.waitJS("legacy-api label", textOf(".flow-svg")+".includes('legacy-api')")
}

func TestErroredCheckoutIsFlagged(t *testing.T) {
	b := open(t)
	b.goTo("/")
	b.waitJS("error dot on checkout", withText(".trace-row", ops.checkout)+"?.querySelector('.dot-error')")
}

func TestHeaderShowsVersion(t *testing.T) {
	b := open(t)
	b.goTo("/")
	if v := b.jsString(textOf(".version")); !strings.HasPrefix(v, "v") || strings.Count(v, ".") != 2 {
		t.Errorf("version %q", v)
	}
}

func TestPauseTogglesTheFeed(t *testing.T) {
	b := open(t)
	b.goTo("/")
	b.waitJS("pause button", textOf("#lp-pause")+" === 'Pause'")
	b.click("#lp-pause")
	b.waitJS("paused label", textOf("#lp-pause")+".startsWith('Paused')")
}

// --- Go UI: live streaming ------------------------------------------------------------------

func liveTrace(id, op string) []core.Event {
	now := time.Now().UnixMilli() * 1000
	return []core.Event{ev(id, id+"-a", "", gateway, "", op, core.KindServer, now, 5*ms, core.StatusOK, nil)}
}

func TestNewTracesStreamIntoTheFeed(t *testing.T) {
	b := open(t)
	b.goTo("/")
	b.waitJS("stream connected", textOf("#lp-status-text")+" === 'live'")
	op := fmt.Sprintf("GET /streamed-%d", time.Now().UnixNano())
	post(t, liveTrace("stream-"+op, op))
	b.waitJS("streamed row", withText(".trace-row", op))
	if top := b.jsString(textOf("#lp-trace-list > li .trace-op")); top != op {
		t.Errorf("newest trace should be first, got %q", top)
	}
}

func TestPauseHoldsAndCountsNewTraces(t *testing.T) {
	b := open(t)
	b.goTo("/")
	b.waitJS("stream connected", textOf("#lp-status-text")+" === 'live'")
	b.click("#lp-pause")
	op := fmt.Sprintf("GET /held-%d", time.Now().UnixNano())
	post(t, liveTrace("held-"+op, op))
	b.waitJS("held count", textOf("#lp-pause")+" === 'Paused · 1 new'")
	if b.jsInt("("+withText(".trace-row", op)+") ? 1 : 0") != 0 {
		t.Fatal("a paused feed must not show new rows")
	}
	b.click("#lp-pause")
	b.waitJS("resumed row", withText(".trace-row", op))
}

func TestZoomSurvivesLiveUpdates(t *testing.T) {
	b := open(t)
	b.goTo("/")
	b.waitJS("stream connected", textOf("#lp-status-text")+" === 'live'")
	b.click("[data-zoom='0.83']")
	zoomed := b.jsString("document.getElementById('lp-flow-svg').getAttribute('viewBox')")
	// Calls to an already-known edge change counts, not the node set → no re-fit.
	post(t, crossService(fmt.Sprintf("zoom-%d", time.Now().UnixNano()), ops.cart, "cart-service", "read", time.Now().UnixMilli()*1000, false))
	time.Sleep(1500 * time.Millisecond) // > one topology tick
	if vb := b.jsString("document.getElementById('lp-flow-svg').getAttribute('viewBox')"); vb != zoomed {
		t.Errorf("viewBox reset by a live tick: %q -> %q", zoomed, vb)
	}
	b.click("[data-fit]")
	b.waitJS("fit restores", "document.getElementById('lp-flow-svg').getAttribute('viewBox') === document.getElementById('lp-flow-svg').dataset.fit")
}

func TestMatrixViewAndFilterLiveInTheURL(t *testing.T) {
	b := open(t)
	b.goTo("/")
	b.clickJS("Matrix toggle", withText(".live-view-toggle a", "Matrix"))
	b.waitURL("?view=matrix", func(u string) bool { return strings.HasSuffix(u, "/?view=matrix") })
	b.waitJS("matrix cells", count(".depmatrix-cell")+" > 0")
	b.clickJS("errors metric", withText(".depmatrix-metric a", "errors"))
	b.waitURL("metric=errors", func(u string) bool { return strings.Contains(u, "metric=errors") })

	b.goTo("/?service=payments")
	b.waitJS("filtered graph", count("#lp-flow-content .flow-node-group")+" === 2") // payments + web-gateway
	b.waitJS("filter selected", "document.getElementById('lp-filter').value === 'payments'")
}
