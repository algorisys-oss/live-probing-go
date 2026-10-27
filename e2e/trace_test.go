package e2e

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Port of e2e/trace.spec.ts, plus N+1 group expansion and the not-found page.

func TestClickingATraceOpensItsWaterfall(t *testing.T) {
	b := open(t)
	b.goTo("/")
	b.clickJS("checkout row", withText(".trace-row", ops.checkout))
	b.waitURL("/trace/"+checkoutID, func(u string) bool { return strings.Contains(u, "/trace/"+checkoutID) })
	b.waitJS("trace page", visibleCount(".trace-page")+" > 0")
	b.waitJS("3+ spans", count(".wf-row")+" >= 3")
}

func TestSequenceTabRendersTheDiagram(t *testing.T) {
	b := open(t)
	b.goTo("/trace/" + checkoutID)
	b.clickJS("Sequence tab", withText(".tab", "Sequence"))
	b.waitJS("seq svg", visibleCount(".seq-svg")+" > 0")
}

func TestSequenceDrawsActivationsAndReturns(t *testing.T) {
	b := open(t)
	b.goTo("/trace/" + checkoutID + "?tab=sequence")
	b.waitJS("activations", visibleCount(".seq-activation")+" > 0")
	b.waitJS("returns", count(".seq-return")+" > 0")
	b.waitJS("shared detail", visibleCount(".wf-detail")+" > 0")
}

func TestWaterfallAxisRailsAndDetail(t *testing.T) {
	b := open(t)
	b.goTo("/trace/" + checkoutID)
	b.waitJS("ticks", visibleCount(".wf-tick-label")+" > 0")
	b.waitJS("rails", visibleCount(".wf-rail")+" > 0")
	// the checkout fails — the shared detail auto-selects the failing span
	b.waitJS("detail names payments", textOf(".wf-detail")+".includes('payments')")
	b.waitJS("detail says error", textOf(".wf-detail")+".includes('error')")
}

func TestSpanSelectionIsSharedAcrossTabs(t *testing.T) {
	b := open(t)
	b.goTo("/trace/" + checkoutID)
	b.clickJS("root row", withText(".wf-row", ops.checkout))
	b.waitJS("root selected", textOf(".wf-detail")+".includes('"+ops.checkout+"')")

	b.clickJS("Sequence tab", withText(".tab", "Sequence"))
	b.waitJS("seq svg", visibleCount(".seq-svg")+" > 0")
	b.waitJS("still the root", textOf(".wf-detail")+".includes('"+ops.checkout+"')")

	// clicking a message arrow selects the span it represents (the failing "charge")
	b.clickJS("first message", "document.querySelector('.seq-msg-clickable')")
	b.waitJS("charge selected", textOf(".wf-detail")+".includes('charge')")

	b.clickJS("Waterfall tab", withText(".tab", "Waterfall"))
	b.waitJS("charge row highlighted", textOf(".wf-row-selected")+".includes('charge')")
}

func TestWaterfallCriticalPathAndSelfTime(t *testing.T) {
	b := open(t)
	b.goTo("/trace/" + checkoutID)
	b.waitJS("critical marks", count(".wf-critical-mark")+" > 0")
	b.waitJS("self-time shading", count(".wf-bar-childtime")+" > 0")
}

func TestCollapsingHidesTheSubtree(t *testing.T) {
	b := open(t)
	b.goTo("/trace/" + checkoutID)
	before := b.jsInt(visibleCount(".wf-row"))
	if before < 3 {
		t.Fatalf("rows: %d", before)
	}
	b.click(".wf-row button.wf-toggle")
	// The React UI removed collapsed rows from the DOM; the Go UI hides them — count visible rows.
	b.waitJS("subtree hidden", visibleCount(".wf-row")+" < "+strconv.Itoa(before))
}

func TestHashDeepLinkSelectsTheSpan(t *testing.T) {
	b := open(t)
	b.goTo("/trace/" + checkoutID + "#" + checkoutID + "-c")
	b.waitJS("charge selected", textOf(".wf-detail")+".includes('charge')")
}

func TestSelectingASpanWritesTheHash(t *testing.T) {
	b := open(t)
	b.goTo("/trace/" + checkoutID)
	b.click(".wf-row")
	b.waitJS("hash set", "location.hash !== ''")
}

func TestBackLinkReturnsToLive(t *testing.T) {
	b := open(t)
	b.goTo("/trace/" + checkoutID)
	b.clickJS("back link", withText("a.back-link", "← Live"))
	b.waitURL("/", func(u string) bool { return strings.HasSuffix(u, "/") })
	b.waitJS("trace list", visibleCount(".trace-list")+" > 0")
}

// --- Go UI extras ---------------------------------------------------------------------------

func TestNPlusOneGroupExpandsAndKeepsSelection(t *testing.T) {
	b := open(t)
	b.goTo("/trace/" + nplusoneID + "?tab=sequence#r")
	b.waitJS("collapsed N+1 row", withText(".seq-msg-group", "⚠ SELECT item ×5"))
	b.clickJS("group row", "document.querySelector('.seq-msg-group')")
	u := b.waitURL("expand=", func(u string) bool { return strings.Contains(u, "expand=") })
	if !strings.HasSuffix(u, "#r") {
		t.Errorf("selection lost on expand: %s", u)
	}
	b.waitJS("lead row", withText(".seq-msg-group", "▾ SELECT item (×5)"))
}

func TestUnknownTraceIs404(t *testing.T) {
	if skipWhy != "" {
		t.Skip("e2e skipped: " + skipWhy)
	}
	res, err := http.Get(baseURL + "/trace/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Errorf("status %d", res.StatusCode)
	}
}
