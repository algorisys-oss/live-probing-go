package e2e

import (
	"regexp"
	"strings"
	"testing"
)

// Ports of e2e/nav.spec.ts and e2e/search.spec.ts, plus the Go UI's other pages.

func TestErrorsPageListsSeededErrorEndpoint(t *testing.T) {
	b := open(t)
	b.goTo("/errors")
	b.waitJS("heading", textOf("h1")+" === 'Errors'")
	b.waitJS("checkout error row", textOf(".errors-table")+".includes('"+ops.checkout+"')")
}

func TestHistoryLinksToADay(t *testing.T) {
	b := open(t)
	b.goTo("/history")
	b.waitJS("heading", textOf("h1")+" === 'History'")
	b.click(".day-link")
	b.waitURL("/day/YYYY-MM-DD", regexp.MustCompile(`/day/\d{4}-\d{2}-\d{2}`).MatchString)
	b.waitJS("rollups", visibleCount(".rollup-card")+" > 0")
}

func TestHistoryChartLabelsAreLegible(t *testing.T) {
	b := open(t)
	b.goTo("/history")
	// Regression: axis labels once fell back to the SVG default black fill.
	if fill := b.jsString("getComputedStyle(document.querySelector('.trends-chart .axis-label')).fill"); fill == "rgb(0, 0, 0)" {
		t.Errorf("axis label fill is black")
	}
}

func TestThemeToggleSwitchesAndPersists(t *testing.T) {
	b := open(t)
	b.goTo("/history")
	b.exec("localStorage.removeItem('liveprobe-theme')")
	b.goTo("/history")
	b.waitJS("dark by default", "document.documentElement.dataset.theme === 'dark'")
	b.click(".theme-toggle")
	b.waitJS("light", "document.documentElement.dataset.theme === 'light'")
	b.waitJS("white body", "getComputedStyle(document.body).backgroundColor === 'rgb(255, 255, 255)'")
	b.goTo("/history") // survives a reload (localStorage, applied pre-paint)
	b.waitJS("still light", "document.documentElement.dataset.theme === 'light'")
	b.exec("localStorage.removeItem('liveprobe-theme')")
}

func searchFor(t *testing.T, b *browser, field, value string) {
	t.Helper()
	b.goTo("/search")
	b.waitJS("heading", textOf("h1")+" === 'Search traces'")
	b.exec("(() => { const i = document.querySelector('.search-form input[name=" + field + "]'); i.value = '" + value + "'; i.form.requestSubmit() })()")
}

func TestSearchByEndpointSubstring(t *testing.T) {
	b := open(t)
	searchFor(t, b, "q", "checkout")
	b.waitJS("result count", visibleCount(".search-count")+" > 0")
	b.waitJS("checkout result", withText(".search-results .trace-row", ops.checkout))
	if u := b.url(); !strings.HasSuffix(u, "/search?q=checkout") {
		t.Errorf("empty fields should be dropped from the URL: %s", u)
	}
}

func TestSearchBySpanAttribute(t *testing.T) {
	b := open(t)
	searchFor(t, b, "attr", "http.status_code=402")
	b.waitJS("checkout result", withText(".search-results .trace-row", ops.checkout))
}

// --- Go UI: the remaining pages ---------------------------------------------------------------

func TestDayPageEndpointCharts(t *testing.T) {
	b := open(t)
	b.goTo("/history")
	b.click(".day-link")
	b.waitJS("endpoints table", withText("tr[data-href]", ops.checkout))
	b.clickJS("checkout endpoint", withText("tr[data-href]", ops.checkout))
	b.waitURL("?endpoint=", func(u string) bool { return strings.Contains(u, "endpoint=") && strings.HasSuffix(u, "#latency") })
	b.waitJS("latency lines", count(".lat-line")+" === 3")
	b.waitJS("histogram", count(".lat-hist-bar")+" === 12")
	b.waitJS("heatmap", count(".heat-cell")+" > 0")
	b.waitJS("flamegraph", count(".flame-cell")+" > 0")
}

func TestAlertsPageListsFiringAlerts(t *testing.T) {
	b := open(t)
	b.goTo("/alerts")
	// The seeded checkout fails in "payments" (100% of its requests) → an error-rate alert.
	b.waitJS("payments alert", withText(".alert-row", "payments error rate"))
	b.waitJS("badge", "!document.getElementById('lp-alert-badge').hidden")
	first := b.jsString(textOf("[data-since]"))
	b.waitJS("timer ticks", textOf("[data-since]")+" !== '"+first+"'")
}

func TestServiceAndComparePages(t *testing.T) {
	b := open(t)
	b.goTo("/service/" + gateway)
	b.waitJS("service cards", count(".card")+" === 3")
	b.waitJS("outbound deps", withText(".svc-deps", "payments"))

	b.goTo("/compare?a=" + checkoutID + "&b=" + cartID)
	b.waitJS("two heads", count(".cmp-head")+" === 2")
	b.waitJS("diff rows", count(".cmp-table tbody tr")+" > 0")
	b.goTo("/compare?a=nope")
	b.waitJS("missing trace", textOf(".error-msg")+" === 'Trace A not found.'")
}
