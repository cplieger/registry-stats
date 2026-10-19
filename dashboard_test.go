package main

import (
	"cmp"
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type dashQuery struct {
	Spec struct {
		Query struct {
			Spec struct {
				Expr string `json:"expr"`
			} `json:"spec"`
		} `json:"query"`
		RefID string `json:"refId"`
	} `json:"spec"`
}

type dashOverride struct {
	Matcher struct {
		ID      string `json:"id"`
		Options any    `json:"options"`
	} `json:"matcher"`
	Properties []dashProperty `json:"properties"`
}

type dashProperty struct {
	ID    string          `json:"id"`
	Value json.RawMessage `json:"value"`
}

// unsetsMin reports whether an override clears the panel's min for the frames
// of query refID: Grafana removes a property whose override has no value.
func unsetsMin(overrides []dashOverride, refID string) bool {
	for _, o := range overrides {
		if o.Matcher.ID != "byFrameRefID" || o.Matcher.Options != refID {
			continue
		}
		for _, p := range o.Properties {
			if p.ID == "min" && (len(p.Value) == 0 || string(p.Value) == "null") {
				return true
			}
		}
	}
	return false
}

type dashTransformation struct {
	Group string `json:"group"`
	Spec  struct {
		Options struct {
			ExcludeByName map[string]bool   `json:"excludeByName"`
			RenameByName  map[string]string `json:"renameByName"`
		} `json:"options"`
	} `json:"spec"`
}

type dashPanel struct {
	Spec struct {
		VizConfig struct {
			Group string `json:"group"`
			Spec  struct {
				Options struct {
					Legend struct {
						Calcs []string `json:"calcs"`
					} `json:"legend"`
					EnablePagination bool   `json:"enablePagination"`
					ColorMode        string `json:"colorMode"`
				} `json:"options"`
				FieldConfig struct {
					Defaults struct {
						Min *float64 `json:"min"`
					} `json:"defaults"`
					Overrides []dashOverride `json:"overrides"`
				} `json:"fieldConfig"`
			} `json:"spec"`
		} `json:"vizConfig"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Data        struct {
			Spec struct {
				Queries         []dashQuery          `json:"queries"`
				Transformations []dashTransformation `json:"transformations"`
			} `json:"spec"`
		} `json:"data"`
	} `json:"spec"`
}

func loadPanels(t *testing.T) map[string]dashPanel {
	t.Helper()
	raw, err := os.ReadFile("grafana-dashboard.json")
	if err != nil {
		t.Fatalf("Setup: read dashboard: %v", err)
	}
	var d struct {
		Spec struct {
			Elements map[string]dashPanel `json:"elements"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("Setup: decode dashboard: %v", err)
	}
	return d.Spec.Elements
}

func panelByTitle(t *testing.T, panels map[string]dashPanel, title string) dashPanel {
	t.Helper()
	for _, p := range panels {
		if p.Spec.Title == title {
			return p
		}
	}
	t.Fatalf("Setup: no panel titled %q", title)
	return dashPanel{}
}

var byClause = regexp.MustCompile(`\b(\w+) by \(([^)]*)\)`)

// perRegistryByDesign are panels whose subject is a registry's own number, so
// they keep the registry whatever Split by registry says.
var perRegistryByDesign = map[string]string{
	"Counts that went down in ${span} ${spanunit}": "a drop is one registry restating its total",
	"Counts that went down":                        "a drop is one registry restating its total",
}

// Every downloads query groups an image by owner and repo, and adds the registry
// only through the Split by registry switch. A total over every package groups by
// (__name__${split}): no series keeps __name__ after an aggregation, so with the
// switch off the total is one series, and with it on one series per registry.
func TestDashboard_DownloadGroupingsFollowTheSplitSwitch(t *testing.T) {
	groupings, totals := 0, 0
	for name, p := range loadPanels(t) {
		if _, ok := perRegistryByDesign[p.Spec.Title]; ok {
			continue
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			e := q.Spec.Query.Spec.Expr
			if !strings.Contains(e, "registrystats_image_pulls_total") {
				continue
			}
			for _, m := range byClause.FindAllStringSubmatch(e, -1) {
				labels := m[2]
				if m[1] == "max" && labels == "owner, repo, registry" {
					continue // the one-copy-per-collector step, which TestDashboard_DownloadReadsCountEachPackageOnce pins
				}
				if !strings.Contains(labels, "owner") && !strings.Contains(labels, "repo") {
					if strings.Contains(labels, "registry") || labels == "__name__${split}" {
						totals++
						if labels != "__name__${split}" {
							t.Errorf("%s %q query %s groups by (%s), want (__name__${split})", name, p.Spec.Title, q.Spec.RefID, labels)
						}
					}
					continue
				}
				groupings++
				if labels != "owner, repo${split}" {
					t.Errorf("%s %q query %s groups by (%s), want (owner, repo${split})", name, p.Spec.Title, q.Spec.RefID, labels)
				}
			}
		}
	}
	if groupings == 0 {
		t.Error("no per-package grouping found; the pattern no longer reads the dashboard")
	}
	if totals == 0 {
		t.Error("no downloads total over every package found; the pattern no longer reads the dashboard")
	}
}

var (
	aggregationCall = regexp.MustCompile(`\b(sum|min|max|avg|count|group|topk|bottomk|quantile|stddev|stdvar) ?(?:by \(([^()]*)\) )?$`)
	pullsSeries     = regexp.MustCompile(`registrystats_image_pulls_total\b`)
)

// innermostAggregation returns the aggregation operator and grouping of the
// innermost aggregation whose parentheses enclose expr[at], or "" when none does.
func innermostAggregation(expr string, at int) (op, by string) {
	depth := 0
	for i := at - 1; i >= 0; i-- {
		switch expr[i] {
		case ')':
			depth++
		case '(':
			if depth > 0 {
				depth--
				continue
			}
			if m := aggregationCall.FindStringSubmatch(expr[:i]); m != nil {
				return m[1], m[2]
			}
		}
	}
	return "", ""
}

// Every collector scraping the same package reports the same upstream count, so
// a download read collapses the job, instance and tenant copies with a max per
// package and registry before anything adds them up.
func TestDashboard_DownloadReadsCountEachPackageOnce(t *testing.T) {
	reads := 0
	for name, p := range loadPanels(t) {
		for _, q := range p.Spec.Data.Spec.Queries {
			e := q.Spec.Query.Spec.Expr
			for _, loc := range pullsSeries.FindAllStringIndex(e, -1) {
				reads++
				op, by := innermostAggregation(e, loc[0])
				if op == "group" || (op == "max" && by == "owner, repo, registry") {
					continue
				}
				t.Errorf("%s %q query %s reads downloads inside %s by (%s), want max by (owner, repo, registry) first",
					name, p.Spec.Title, q.Spec.RefID, cmp.Or(op, "no aggregation"), by)
			}
		}
	}
	if reads == 0 {
		t.Error("no download read found; the pattern no longer reads the dashboard")
	}
}

var changeCall = regexp.MustCompile(`\b(delta|changes)\(`)

// A collector replaced partway through a window (a new instance label) splits a
// package's growth across two series, so every change is taken over the
// subquery of the already collapsed package series, never per series.
func TestDashboard_DownloadChangesSpanCollectorReplacements(t *testing.T) {
	const collapsed = "(max by (owner, repo, registry) (registrystats_image_pulls_total"
	changes := 0
	for name, p := range loadPanels(t) {
		for _, q := range p.Spec.Data.Spec.Queries {
			e := q.Spec.Query.Spec.Expr
			if !strings.Contains(e, "registrystats_image_pulls_total") {
				continue
			}
			for _, loc := range changeCall.FindAllStringIndex(e, -1) {
				changes++
				if !strings.HasPrefix(e[loc[1]:], collapsed) {
					t.Errorf("%s %q query %s takes %s over %.60q, want a subquery of %s...)",
						name, p.Spec.Title, q.Spec.RefID, e[loc[0]:loc[1]-1], e[loc[1]:], collapsed)
				}
			}
		}
	}
	if changes == 0 {
		t.Error("no download change found; the pattern no longer reads the dashboard")
	}
}

// onMatch captures the labels of an on (...) match and the grouping of the
// aggregation that forms its right operand.
var onMatch = regexp.MustCompile(`on \(([^)]+)\)(?: group_left \([^)]*\))? \(*(?:max|min|sum|count|group) by \(([^)]*)\)`)

// Every on (...) match keeps all of its labels in the right operand's grouping;
// a dropped label would make the join silently match nothing.
func TestDashboard_OnMatchesKeepTheirLabels(t *testing.T) {
	matches := 0
	for name, p := range loadPanels(t) {
		for _, q := range p.Spec.Data.Spec.Queries {
			e := strings.ReplaceAll(q.Spec.Query.Spec.Expr, "${split}", ", registry")
			for _, m := range onMatch.FindAllStringSubmatch(e, -1) {
				matches++
				kept := strings.Split(m[2], ", ")
				for l := range strings.SplitSeq(m[1], ", ") {
					if !slices.Contains(kept, l) {
						t.Errorf("%s %q matches on %s but its right operand keeps only %v", name, p.Spec.Title, l, kept)
					}
				}
			}
		}
	}
	if matches == 0 {
		t.Error("no on (...) match found; the pattern no longer reads the dashboard")
	}
}

// A signed change goes below zero when a registry restates its total lower, so a
// panel min that a count beside it needs is cleared for the change's own frames.
func TestDashboard_SignedChangesAreNotClippedAtAMinimum(t *testing.T) {
	for id, p := range loadPanels(t) {
		fc := p.Spec.VizConfig.Spec.FieldConfig
		if fc.Defaults.Min == nil {
			continue
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			expr := q.Spec.Query.Spec.Expr
			signed := strings.Contains(expr, "delta(") && !strings.Contains(expr, "clamp_min(")
			if signed && !unsetsMin(fc.Overrides, q.Spec.RefID) {
				t.Errorf("%s %q sets min %v and no byFrameRefID override clears it for query %s, which takes a signed delta: %s",
					id, p.Spec.Title, *fc.Defaults.Min, q.Spec.RefID, expr)
			}
		}
	}
}

var (
	changeWindow = regexp.MustCompile(`\b(?:delta|changes)\(\(max by \(owner, repo, registry\) \(registrystats_image_pulls_total\{[^}]*\}\)\)\[([^:\]]+):([^\]]+)\]`)
	changeOffset = regexp.MustCompile(`\b(?:delta|changes)\(\(max by \(owner, repo, registry\) \(registrystats_image_pulls_total\{[^}]*\}\)\)\[[^\]]+\] offset ([^)\s]+)\)`)
	rangeBefore  = regexp.MustCompile(`\[\$__range:[^\]]+\] offset \$__range\)|offset \$__range\)+\[[^\]]+\] offset \$__range\)`)
	earlierGuard = regexp.MustCompile(`registrystats_image_pulls_total\{[^}]*\} offset \$__range\)\)\[1d:1h\] offset \$__range\)`)
)

// A tile or table states the downloads of the range the reader picked, and a
// comparison is against the range of the same length just before it, so no
// download figure outside a chart's own bars carries a fixed window.
func TestDashboard_DownloadFiguresFollowTheTimeRange(t *testing.T) {
	windows := 0
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group == "timeseries" {
			continue // a bar's window is its own bucket, matched to the query interval
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			e := q.Spec.Query.Spec.Expr
			for _, m := range changeWindow.FindAllStringSubmatch(e, -1) {
				windows++
				if m[1] != "$__range" {
					t.Errorf("%s %q query %s takes a change over [%s], want [$__range]", name, p.Spec.Title, q.Spec.RefID, m[1])
				}
			}
			for _, m := range changeOffset.FindAllStringSubmatch(e, -1) {
				if m[1] != "$__range" {
					t.Errorf("%s %q query %s compares against offset %s, want offset $__range", name, p.Spec.Title, q.Spec.RefID, m[1])
				}
			}
		}
	}
	if windows == 0 {
		t.Error("no download change found outside the charts; the pattern no longer reads the dashboard")
	}
}

// Data that starts inside the range before would make that range look small and
// the change look huge, so every comparison is held back until a package was
// reported in the day before the range before began. A guard's own tail matches
// rangeBefore, so each read of the earlier span beyond the guards needs one.
func TestDashboard_RangeComparisonsWaitForAWholeEarlierRange(t *testing.T) {
	comparisons := 0
	for name, p := range loadPanels(t) {
		for _, q := range p.Spec.Data.Spec.Queries {
			e := q.Spec.Query.Spec.Expr
			guards := len(earlierGuard.FindAllString(e, -1))
			reads := len(rangeBefore.FindAllString(e, -1)) - guards
			if reads <= 0 {
				continue
			}
			comparisons++
			if guards < reads {
				t.Errorf("%s %q query %s reads the range before %d times with %d checks that the data reaches back to its start: %.160s",
					name, p.Spec.Title, q.Spec.RefID, reads, guards, e)
			}
		}
	}
	if comparisons == 0 {
		t.Error("no comparison with the range before found; the pattern no longer reads the dashboard")
	}
}

var fixedWindow = regexp.MustCompile(`\[\d+[smhdwy][\]:]|offset \d+[smhdwy]\b`)

// A bar holds the downloads of its own span, which Grafana sizes from the range,
// so a fixed window would put a short range's only bar before the range began.
func TestDashboard_DownloadBarsSpanTheQueryInterval(t *testing.T) {
	charts := 0
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group != "timeseries" {
			continue
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			e := q.Spec.Query.Spec.Expr
			if !strings.Contains(e, "registrystats_image_pulls_total") {
				continue
			}
			charts++
			if w := fixedWindow.FindString(e); w != "" || !strings.Contains(e, "[$__interval:") {
				t.Errorf("%s %q query %s spans %q, want a change over [$__interval:...]: %.160s",
					name, p.Spec.Title, q.Spec.RefID, cmp.Or(w, "no $__interval"), e)
			}
		}
	}
	if charts == 0 {
		t.Error("no download chart found; the pattern no longer reads the dashboard")
	}
}

// A subquery's change extrapolates from its first sample to the window start, so its
// step biases a figure by step / range: a range figure samples every minute, the
// scrape cadence, which keeps a 6 h figure within a few tenths of a percent.
func TestDashboard_RangeFiguresSampleEveryMinute(t *testing.T) {
	figures := 0
	for name, p := range loadPanels(t) {
		for _, q := range p.Spec.Data.Spec.Queries {
			for _, m := range changeWindow.FindAllStringSubmatch(q.Spec.Query.Spec.Expr, -1) {
				if m[1] != "$__range" {
					continue
				}
				figures++
				if m[2] != "1m" {
					t.Errorf("%s %q query %s samples [$__range:%s], want [$__range:1m]", name, p.Spec.Title, q.Spec.RefID, m[2])
				}
			}
		}
	}
	if figures == 0 {
		t.Error("no range figure found; the pattern no longer reads the dashboard")
	}
}

// inRange drops a sample at or before the range start. Grafana aligns a range query's
// start down to its step, so the first sample closes the bucket before the range: off
// the axis, yet counted by a tooltip or a legend calculation.
const inRange = " and on () (vector(time()) > $__from / 1000)"

// Every bar's bucket ends inside the range, so a bar chart query ends with inRange.
func TestDashboard_BarsCountOnlyBucketsInsideTheRange(t *testing.T) {
	bars := 0
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group != "timeseries" {
			continue
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			e := q.Spec.Query.Spec.Expr
			if !strings.Contains(e, "[$__interval") {
				continue
			}
			bars++
			if !strings.HasSuffix(e, inRange) {
				t.Errorf("%s %q query %s keeps the bucket before the range, want it to end with %q: %.160s",
					name, p.Spec.Title, q.Spec.RefID, inRange, e)
			}
		}
	}
	if bars == 0 {
		t.Error("no bar chart query found; the pattern no longer reads the dashboard")
	}
}

// Bars cover whole aligned buckets, which start before the range and stop short of its
// end, so a legend total over them disagrees with the Downloads tile.
func TestDashboard_DownloadBarLegendsCarryNoTotal(t *testing.T) {
	charts := 0
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group != "timeseries" || len(p.Spec.Data.Spec.Queries) == 0 ||
			!strings.Contains(p.Spec.Data.Spec.Queries[0].Spec.Query.Spec.Expr, "registrystats_image_pulls_total") {
			continue
		}
		charts++
		if calcs := p.Spec.VizConfig.Spec.Options.Legend.Calcs; len(calcs) > 0 {
			t.Errorf("%s %q legend calculates %v, want no calculation", name, p.Spec.Title, calcs)
		}
	}
	if charts == 0 {
		t.Error("no download chart found; the pattern no longer reads the dashboard")
	}
}

const (
	// phoneTableWidth is the table width a full-width panel keeps on a 412 px
	// phone viewport, measured in Grafana 13.2.
	phoneTableWidth = 338
	// unsizedColumnWidth is the width Grafana gives a column that states none.
	unsizedColumnWidth = 150
)

// columnFloor is the width a table column takes at least: its custom.width or
// custom.minWidth override, else unsizedColumnWidth.
func columnFloor(overrides []dashOverride, column string) int {
	for _, o := range overrides {
		if o.Matcher.ID != "byName" || o.Matcher.Options != column {
			continue
		}
		for _, p := range o.Properties {
			if p.ID != "custom.width" && p.ID != "custom.minWidth" {
				continue
			}
			var w int
			if err := json.Unmarshal(p.Value, &w); err == nil {
				return w
			}
		}
	}
	return unsizedColumnWidth
}

func hiddenColumn(overrides []dashOverride, column string) bool {
	for _, o := range overrides {
		if o.Matcher.ID != "byName" || o.Matcher.Options != column {
			continue
		}
		for _, p := range o.Properties {
			if p.ID == "custom.hidden" && string(p.Value) == "true" {
				return true
			}
		}
	}
	return false
}

// A table wider than a phone scrolls sideways and cuts the column at its edge, so
// the floors of every shown column fit phoneTableWidth. The shown columns are the
// ones the organize transformation names and neither excludes nor hides.
func TestDashboard_TablesFitAPhone(t *testing.T) {
	tables := 0
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group != "table" {
			continue
		}
		tables++
		overrides := p.Spec.VizConfig.Spec.FieldConfig.Overrides
		width, shown := 0, []string{}
		for _, tr := range p.Spec.Data.Spec.Transformations {
			if tr.Group != "organize" {
				continue
			}
			for field, column := range tr.Spec.Options.RenameByName {
				if tr.Spec.Options.ExcludeByName[field] || hiddenColumn(overrides, column) {
					continue
				}
				width += columnFloor(overrides, column)
				shown = append(shown, column)
			}
		}
		if width > phoneTableWidth {
			slices.Sort(shown)
			t.Errorf("%s %q shows %v at %d px of column floors, want at most %d", name, p.Spec.Title, shown, width, phoneTableWidth)
		}
	}
	if tables == 0 {
		t.Error("no table found; the pattern no longer reads the dashboard")
	}
}

// relativeTimeColumnPx fits "a few seconds ago", the longest time the dateTimeFromNow unit
// prints: 127.4 px of 14 px Inter plus 6 px of cell padding each side, measured in Grafana
// 13.2.3. The column is right-aligned, so a narrower one cuts the start of the text.
const relativeTimeColumnPx = 140

// Every "x minutes ago" column is wide enough for the longest relative time.
func TestDashboard_RelativeTimeColumnsFitTheLongestTime(t *testing.T) {
	columns := 0
	for name, p := range loadPanels(t) {
		overrides := p.Spec.VizConfig.Spec.FieldConfig.Overrides
		for _, o := range overrides {
			relative := slices.ContainsFunc(o.Properties, func(p dashProperty) bool {
				return p.ID == "unit" && string(p.Value) == `"dateTimeFromNow"`
			})
			column, _ := o.Matcher.Options.(string)
			if !relative || o.Matcher.ID != "byName" {
				continue
			}
			columns++
			if w := columnFloor(overrides, column); w < relativeTimeColumnPx {
				t.Errorf("%s %q column %q is %d px wide, want at least %d for %q", name, p.Spec.Title, column, w, relativeTimeColumnPx, "a few seconds ago")
			}
		}
	}
	if columns == 0 {
		t.Error("no relative time column found; the pattern no longer reads the dashboard")
	}
}

var rowCap = regexp.MustCompile(`\b(?:topk|bottomk)\((\d+),`)

// scrollingByDesign are tables that list every package in one fixed-height box,
// so their row count follows the install and the reader scrolls them.
var scrollingByDesign = map[string]string{
	"Package downloads in ${span} ${spanunit}": "every tracked package in one compact list",
}

// A panel's height is fixed, so a table whose row count follows the range or Split
// by registry hides rows behind a scroll and cuts the one at its edge. Grafana sizes
// a page to whole rows, so every table whose query does not cap its rows pages.
func TestDashboard_UncappedTablesPageWholeRows(t *testing.T) {
	uncapped := 0
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group != "table" || len(p.Spec.Data.Spec.Queries) == 0 {
			continue
		}
		if _, ok := scrollingByDesign[p.Spec.Title]; ok {
			continue
		}
		expr := p.Spec.Data.Spec.Queries[0].Spec.Query.Spec.Expr
		if rowCap.MatchString(expr) {
			continue
		}
		uncapped++
		if !p.Spec.VizConfig.Spec.Options.EnablePagination {
			t.Errorf("%s %q has no row cap in its query and does not enable pagination: %.120s", name, p.Spec.Title, expr)
		}
	}
	if uncapped == 0 {
		t.Error("no uncapped table found; the pattern no longer reads the dashboard")
	}
}

func loadDoc(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("grafana-dashboard.json")
	if err != nil {
		t.Fatalf("Setup: read dashboard: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("Setup: decode dashboard: %v", err)
	}
	return doc
}

func autoGridRules(t *testing.T) map[string]any {
	t.Helper()
	rules := map[string]any{}
	for _, item := range autoGridItems(t) {
		name, _ := dig(item, "element", "name").(string)
		rules[name] = item["conditionalRendering"]
	}
	return rules
}

// autoGridItems returns the spec of every AutoGridLayoutItem in the dashboard.
func autoGridItems(t *testing.T) []map[string]any {
	t.Helper()
	var items []map[string]any
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if n["kind"] == "AutoGridLayoutItem" {
				spec, _ := n["spec"].(map[string]any)
				items = append(items, spec)
			}
			for _, child := range n {
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(loadDoc(t))
	return items
}

func ruleKinds(rule any) []string {
	items, _ := dig(rule, "spec", "items").([]any)
	kinds := make([]string, 0, len(items))
	for _, it := range items {
		kind, _ := dig(it, "kind").(string)
		kinds = append(kinds, kind)
	}
	return kinds
}

// startsHiddenUntilData returns why rule does not keep its panel hidden until the query
// returns data, or "". Grafana 13.2.3 shows an item while its has-data rule is undecided,
// on load and on every refresh slower than 200 ms (grafana/grafana#134713), so an "any"
// group adds a range-size rule that is always false. A display: none panel measures no
// width, so its query runs only with a pinned maxDataPoints.
func startsHiddenUntilData(rule any, panel map[string]any) string {
	items, _ := dig(rule, "spec", "items").([]any)
	if dig(rule, "kind") != "ConditionalRenderingGroup" || dig(rule, "spec", "visibility") != "show" ||
		dig(rule, "spec", "condition") != "or" || len(items) != 2 {
		return "want a show group matching any of two rules"
	}
	hasData, neverTrue := false, false
	for _, it := range items {
		switch dig(it, "kind") {
		case "ConditionalRenderingData":
			hasData = dig(it, "spec", "value") == true
		case "ConditionalRenderingTimeRangeSize":
			neverTrue = dig(it, "spec", "value") == "0s"
		}
	}
	if !hasData || !neverTrue {
		return `want a has-data rule and a time range size rule of "0s"`
	}
	if points, _ := dig(panel, "data", "spec", "queryOptions", "maxDataPoints").(float64); points < 1 || points != float64(int(points)) {
		return "want a positive whole queryOptions.maxDataPoints on the panel"
	}
	return ""
}

var badCaseFilter = regexp.MustCompile(`\s(?:[<>]=?|[!=]=)\s+-?\d|\bunless\b`)

// hiddenTables are the tables whose normal state is empty, so they show only with rows.
var hiddenTables = []string{"Not pulled in ${span} ${spanunit}"}

// isAttentionTile reports whether p is an attention tile: a stat painted on a coloured background.
func isAttentionTile(p dashPanel) bool {
	return p.Spec.VizConfig.Group == "stat" && p.Spec.VizConfig.Spec.Options.ColorMode == "background"
}

// hiddenPanels returns the attention tiles and the hiddenTables.
func hiddenPanels(t *testing.T) map[string]dashPanel {
	t.Helper()
	hidden := map[string]dashPanel{}
	for name, p := range loadPanels(t) {
		if isAttentionTile(p) || slices.Contains(hiddenTables, p.Spec.Title) {
			hidden[name] = p
		}
	}
	return hidden
}

// An attention tile or a hidden table stays hidden until its condition holds: Grafana
// honours a "has data" rule only on an auto-grid item, and a query that returns a 0 or
// a row for the healthy case would keep the panel on screen.
func TestDashboard_HiddenPanelsShowOnlyWhileTheirConditionHolds(t *testing.T) {
	rules := autoGridRules(t)
	specs := panelSpecs(t)
	tiles := 0
	hidden := hiddenPanels(t)
	for _, title := range hiddenTables {
		panelByTitle(t, loadPanels(t), title)
	}
	for name, p := range hidden {
		tiles++
		rule, placed := rules[name]
		if !placed {
			t.Errorf("%s %q is not placed by an AutoGridLayoutItem, so no rule can hide it", name, p.Spec.Title)
		} else if why := startsHiddenUntilData(rule, specs[name]); why != "" {
			t.Errorf("%s %q conditionalRendering %v: %s", name, p.Spec.Title, ruleKinds(rule), why)
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			if e := q.Spec.Query.Spec.Expr; !badCaseFilter.MatchString(e) {
				t.Errorf("%s %q query %s returns data in the healthy case too: %.160s", name, p.Spec.Title, q.Spec.RefID, e)
			}
		}
	}
	if tiles <= len(hiddenTables) {
		t.Error("no attention tile found; the pattern no longer reads the dashboard")
	}
}

// Any auto-grid item a has-data rule gates, attention tile or not, starts hidden.
func TestDashboard_DataGatedItemsStartHidden(t *testing.T) {
	specs := panelSpecs(t)
	gated := 0
	for _, item := range autoGridItems(t) {
		rule := item["conditionalRendering"]
		if !slices.Contains(ruleKinds(rule), "ConditionalRenderingData") {
			continue
		}
		gated++
		name, _ := dig(item, "element", "name").(string)
		if why := startsHiddenUntilData(rule, specs[name]); why != "" {
			t.Errorf("%s %v conditionalRendering %v: %s", name, specs[name]["title"], ruleKinds(rule), why)
		}
	}
	if gated == 0 {
		t.Error("no auto-grid item gated by a has-data rule found; the pattern no longer reads the dashboard")
	}
}

// Without preload a lit tile below the fold sits in a lazy loader: it skips a refresh until
// it is scrolled to, then queries, blinks and reflows the panels under it.
func TestDashboard_PreloadsSoGatedPanelsBelowTheFoldRefresh(t *testing.T) {
	if preload := dig(loadDoc(t), "spec", "preload"); preload != true {
		t.Errorf("spec.preload = %v, want true", preload)
	}
}

type tabLink struct {
	title  string
	tab    string
	macros []string
	sets   url.Values
}

// tabLinks returns every panel link that opens a tab of this dashboard, by panel name:
// one to ${__url.path}, or to /d/ and this dashboard's uid.
func tabLinks(t *testing.T) map[string][]tabLink {
	t.Helper()
	uid, _ := dig(loadDoc(t), "metadata", "name").(string)
	links := map[string][]tabLink{}
	for name, spec := range panelSpecs(t) {
		raw, _ := spec["links"].([]any)
		for _, l := range raw {
			target, _ := dig(l, "url").(string)
			path, query, _ := strings.Cut(target, "?")
			if path != "${__url.path}" && path != "/d/"+uid && !strings.HasPrefix(path, "/d/"+uid+"/") {
				continue
			}
			link := tabLink{sets: url.Values{}}
			link.title, _ = dig(l, "title").(string)
			for part := range strings.SplitSeq(query, "&") {
				if strings.HasPrefix(part, "${") {
					link.macros = append(link.macros, part)
					continue
				}
				values, err := url.ParseQuery(part)
				if err != nil {
					t.Fatalf("%s link %q: parse %q: %v", name, link.title, part, err)
				}
				for k, v := range values {
					link.sets[k] = append(link.sets[k], v...)
				}
			}
			link.tab = link.sets.Get("dtab")
			links[name] = append(links[name], link)
		}
	}
	return links
}

var spaceRun = regexp.MustCompile(` +`)

// tabSlugs returns each tab's dtab value and the dtab of the tab holding each panel.
// Grafana 13.2.3 turns each run of spaces in the title into a dash, and a later tab
// repeating an earlier slug gets __2, __3 and so on (getSlugForRowOrTab,
// public/app/features/dashboard-scene/utils/utils.ts).
func tabSlugs(doc map[string]any) (slugs []string, tabOf map[string]string) {
	tabs, _ := dig(doc, "spec", "layout", "spec", "tabs").([]any)
	repeats := map[string]int{}
	tabOf = map[string]string{}
	for _, tab := range tabs {
		title, _ := dig(tab, "spec", "title").(string)
		slug := spaceRun.ReplaceAllString(title, "-")
		repeats[slug]++
		if n := repeats[slug]; n > 1 {
			slug += "__" + strconv.Itoa(n)
		}
		slugs = append(slugs, slug)
		for _, name := range elementNames(tab) {
			tabOf[name] = slug
		}
	}
	return slugs, tabOf
}

func elementNames(v any) []string {
	var names []string
	switch n := v.(type) {
	case map[string]any:
		if name, ok := n["name"].(string); ok && n["kind"] == "ElementReference" {
			names = append(names, name)
		}
		for _, child := range n {
			names = append(names, elementNames(child)...)
		}
	case []any:
		for _, child := range n {
			names = append(names, elementNames(child)...)
		}
	}
	return names
}

// A link to another tab keeps the time range and every filter on screen, so the tab
// it opens shows the same packages over the same range. A switch it sets literally
// must hold one of the switch's own values, or the tab opens with the switch off.
func TestDashboard_TabLinksKeepTheRangeAndFilters(t *testing.T) {
	doc := loadDoc(t)
	slugs, tabOf := tabSlugs(doc)
	docVars, _ := dig(doc, "spec", "variables").([]any)
	checked := 0
	for name, links := range tabLinks(t) {
		for _, link := range links {
			checked++
			if !slices.Contains(slugs, link.tab) {
				t.Errorf("%s link %q opens dtab=%q, want one of %v", name, link.title, link.tab, slugs)
			}
			if link.tab == tabOf[name] {
				t.Errorf("%s link %q opens dtab=%q, the tab the panel is on", name, link.title, link.tab)
			}
			if !slices.Contains(link.macros, "${__url_time_range}") {
				t.Errorf("%s link %q does not carry ${__url_time_range}", name, link.title)
			}
			all := slices.Contains(link.macros, "${__all_variables}")
			for _, v := range docVars {
				spec, _ := dig(v, "spec").(map[string]any)
				varName, _ := spec["name"].(string)
				set, literal := link.sets["var-"+varName]
				if literal && dig(v, "kind") == "SwitchVariable" &&
					set[0] != spec["enabledValue"] && set[0] != spec["disabledValue"] {
					t.Errorf("%s link %q sets var-%s=%q, want the switch's enabledValue %q or disabledValue %q",
						name, link.title, varName, set[0], spec["enabledValue"], spec["disabledValue"])
				}
				carried := all || literal || slices.Contains(link.macros, "${"+varName+":queryparam}")
				if spec["hide"] == "dontHide" && !carried {
					t.Errorf("%s link %q drops the %s filter: want ${__all_variables}, ${%s:queryparam} or var-%s",
						name, link.title, varName, varName, varName)
				}
			}
		}
	}
	if checked == 0 {
		t.Error("no link to a tab found; the pattern no longer reads the dashboard")
	}
}

// Missing from a registry is a count; the packages behind it are the single-registry
// rows of Package downloads, which exist only with Split by registry on.
func TestDashboard_MissingFromARegistryLinkSplitsByRegistry(t *testing.T) {
	doc := loadDoc(t)
	_, split := variable(doc, "split")
	_, tabOf := tabSlugs(doc)
	panels := loadPanels(t)
	detailTab := ""
	for name, p := range panels {
		if strings.HasPrefix(p.Spec.Title, "Package downloads in ") {
			detailTab = tabOf[name]
		}
	}
	if detailTab == "" {
		t.Fatal(`Setup: no tab holds a panel titled "Package downloads in ..."`)
	}
	links := tabLinks(t)
	for name, p := range panels {
		if p.Spec.Title != "Missing from a registry" {
			continue
		}
		if len(links[name]) != 1 {
			t.Fatalf("%s has %d links to a tab, want 1", name, len(links[name]))
		}
		link := links[name][0]
		if link.tab != detailTab {
			t.Errorf("link %q opens dtab=%q, want %q, the tab holding Package downloads", link.title, link.tab, detailTab)
		}
		if got := link.sets["var-split"]; len(got) != 1 || got[0] != split["enabledValue"] {
			t.Errorf("link %q sets var-split to %q, want the switch's enabledValue %q", link.title, got, split["enabledValue"])
		}
		return
	}
	t.Fatal(`Setup: no panel titled "Missing from a registry"`)
}

func rowsPlacing(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	rows := map[string]map[string]any{}
	var walk func(v any, row map[string]any)
	walk = func(v any, row map[string]any) {
		switch n := v.(type) {
		case map[string]any:
			spec, _ := n["spec"].(map[string]any)
			if n["kind"] == "RowsLayoutRow" {
				row = spec
			}
			if n["kind"] == "AutoGridLayoutItem" {
				element, _ := spec["element"].(map[string]any)
				name, _ := element["name"].(string)
				rows[name] = row
			}
			for _, child := range n {
				walk(child, row)
			}
		case []any:
			for _, child := range n {
				walk(child, row)
			}
		}
	}
	walk(doc, nil)
	return rows
}

func variableRule(rule any) string {
	group, _ := rule.(map[string]any)
	spec, _ := group["spec"].(map[string]any)
	items, _ := spec["items"].([]any)
	if group["kind"] != "ConditionalRenderingGroup" || spec["visibility"] != "show" || len(items) != 1 {
		return ""
	}
	item, _ := items[0].(map[string]any)
	itemSpec, _ := item["spec"].(map[string]any)
	name, _ := itemSpec["variable"].(string)
	if item["kind"] != "ConditionalRenderingVariable" || itemSpec["operator"] != "matches" || itemSpec["value"] != "^[1-9]" {
		return ""
	}
	return name
}

func variable(doc map[string]any, name string) (kind string, spec map[string]any) {
	docSpec, _ := doc["spec"].(map[string]any)
	vars, _ := docSpec["variables"].([]any)
	for _, v := range vars {
		vm, _ := v.(map[string]any)
		vs, _ := vm["spec"].(map[string]any)
		if vs["name"] == name {
			kind, _ = vm["kind"].(string)
			return kind, vs
		}
	}
	return "", nil
}

func variableQuery(doc map[string]any, name string) string {
	_, vs := variable(doc, name)
	q, _ := vs["query"].(map[string]any)
	qs, _ := q["spec"].(map[string]any)
	text, _ := qs["__legacyStringValue"].(string)
	return text
}

// Hidden panels live in a row that a hidden variable shows, because an empty row
// still draws its header. The variable counts every panel's own query and nothing
// else, so the row shows exactly while one of its panels has data.
func TestDashboard_HiddenPanelRowsShowWhileAnyPanelHasData(t *testing.T) {
	doc := loadDoc(t)
	rows := rowsPlacing(t, doc)
	tiles := 0
	arms := map[string]int{}
	for name, p := range hiddenPanels(t) {
		tiles++
		row := rows[name]
		variable := variableRule(row["conditionalRendering"])
		if variable == "" {
			t.Errorf("%s %q sits in row %q whose conditionalRendering = %v, want a show rule matching a variable against ^[1-9]",
				name, p.Spec.Title, row["title"], row["conditionalRendering"])
			continue
		}
		counted := variableQuery(doc, variable)
		for _, q := range p.Spec.Data.Spec.Queries {
			arms[variable]++
			if want := "count(" + q.Spec.Query.Spec.Expr + ")"; !strings.Contains(counted, want) {
				t.Errorf("%s %q query %s is not counted by variable %s: want %.120s in %.120s",
					name, p.Spec.Title, q.Spec.RefID, variable, want, counted)
			}
		}
	}
	if tiles <= len(hiddenTables) {
		t.Error("no attention tile found; the pattern no longer reads the dashboard")
	}
	for variable, want := range arms {
		if got := strings.Count(variableQuery(doc, variable), `, "tile", "`); got != want {
			t.Errorf("variable %s counts %d arms, want %d, one per hidden panel query", variable, got, want)
		}
	}
}

// A header that folds one chart or table costs a click and hides whether more sits behind
// it, so a row whose header shows holds at least two always-visible panels. A row of
// attention tiles is exempt: each shows only with data, and together they light several.
func TestDashboard_RowHeadersFoldSeveralPanels(t *testing.T) {
	var rows []map[string]any
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if spec, _ := n["spec"].(map[string]any); n["kind"] == "RowsLayoutRow" {
				rows = append(rows, spec)
			}
			for _, child := range n {
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(loadDoc(t))
	if len(rows) == 0 {
		t.Fatal("no row found; the pattern no longer reads the dashboard")
	}
	panels := loadPanels(t)
	for _, row := range rows {
		if row["hideHeader"] == true {
			continue
		}
		items := rowItems(row)
		shown, tiles := 0, 0
		for _, it := range items {
			rule := dig(it, "spec", "conditionalRendering")
			name, _ := dig(it, "spec", "element", "name").(string)
			switch {
			case rule == nil:
				shown++
			case showsOnData(rule) && isAttentionTile(panels[name]):
				tiles++
			}
		}
		if shown < 2 && (shown > 0 || tiles < 2 || tiles != len(items)) {
			t.Errorf("row %q shows its header over %d always-visible panels and %d attention tiles of %d, "+
				"want two always-visible panels, a row of attention tiles, or hideHeader",
				row["title"], shown, tiles, len(items))
		}
	}
}

// rowItems returns the layout items a row folds, its nested rows' items included.
func rowItems(row any) []any {
	layout := dig(row, "layout")
	if dig(layout, "kind") != "RowsLayout" {
		items, _ := dig(layout, "spec", "items").([]any)
		return items
	}
	rows, _ := dig(layout, "spec", "rows").([]any)
	var items []any
	for _, nested := range rows {
		items = append(items, rowItems(dig(nested, "spec"))...)
	}
	return items
}

// showsOnData reports whether rule holds a has-data rule that shows its item.
func showsOnData(rule any) bool {
	items, _ := dig(rule, "spec", "items").([]any)
	for _, it := range items {
		if dig(it, "kind") == "ConditionalRenderingData" && dig(it, "spec", "value") == true {
			return true
		}
	}
	return false
}

// rangeWordVars are the variables a title names the range's length with.
var rangeWordVars = []string{"span", "spanunit", "spanthe"}

// customText is the text Grafana gives a custom variable whose interpolated query
// is query, which titles render. The parse follows
// CustomVariable.transformCsvStringToOptions,
// https://github.com/grafana/scenes/blob/main/packages/scenes/src/variables/variants/CustomVariable.tsx.
// More than one option would let Grafana keep a stale choice, so it fails.
func customText(t *testing.T, name, query string) string {
	t.Helper()
	entries := regexp.MustCompile(`(?:\\,|[^,])+`).FindAllString(query, -1)
	if len(entries) != 1 {
		t.Errorf("variable %s query %q gives %d options, want 1", name, query, len(entries))
		return ""
	}
	if m := regexp.MustCompile(`^\s*(.+)\s:\s(.+)$`).FindStringSubmatch(entries[0]); m != nil {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(entries[0])
}

// rangeWords is the text of each range word variable once spanread holds the
// option Grafana makes of row, or none at all (""), as on a failed first read.
func rangeWords(t *testing.T, doc map[string]any, row string) map[string]string {
	t.Helper()
	_, read := variable(doc, "spanread")
	pattern, _ := read["regex"].(string)
	re, err := regexp.Compile(strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/"))
	if err != nil || re.SubexpIndex("value") < 0 || re.SubexpIndex("text") < 0 {
		t.Fatalf("spanread regex %q: %v, want value and text groups", pattern, err)
	}
	var value, text string
	if row != "" {
		m := re.FindStringSubmatch(row)
		if m == nil {
			t.Fatalf("spanread regex %q does not match %s", pattern, row)
		}
		value, text = m[re.SubexpIndex("value")], m[re.SubexpIndex("text")]
	}
	fill := strings.NewReplacer("${spanread:text}", text, "${spanread}", value)
	words := map[string]string{}
	for _, name := range rangeWordVars {
		_, vs := variable(doc, name)
		query, _ := vs["query"].(string)
		words[name] = customText(t, name, fill.Replace(query))
	}
	return words
}

// A figure over the dashboard time range reads only with the range's length, so every
// panel outside the charts whose query reads $__range names it, "Downloads in 30 days" or
// "Change over the 30 days", and reads "... the range" until the length is read or when its
// first read fails. Every word comes from the one answer of spanread, so no title mixes two
// reads ("in 30 range") while they arrive. The lengths are spanread's query run at each
// side of every unit's threshold and at decimal roundings.
func TestDashboard_RangeFiguresNameTheirSpan(t *testing.T) {
	doc := loadDoc(t)
	kind, read := variable(doc, "spanread")
	if kind != "QueryVariable" || read["refresh"] != "onTimeRangeChanged" || read["hide"] != "hideVariable" {
		t.Fatalf("spanread is %s refresh %v hide %v, want a hidden QueryVariable refreshed on time range change",
			cmp.Or(kind, "missing"), read["refresh"], read["hide"])
	}
	if current, _ := read["current"].(map[string]any); current["text"] != "" || current["value"] != "" {
		t.Errorf("spanread saves %v, want it empty so a failed first read leaves the fallback", current)
	}
	docSpec, _ := doc["spec"].(map[string]any)
	vars, _ := docSpec["variables"].([]any)
	for _, v := range vars {
		name, _ := dig(v, "spec", "name").(string)
		if q, _ := json.Marshal(dig(v, "spec", "query")); name != "spanread" && strings.Contains(string(q), "$__range_s") {
			t.Errorf("variable %s reads $__range_s, want spanread the only read of the range length", name)
		}
	}
	if !strings.Contains(variableQuery(doc, "spanread"), "$__range_s") {
		t.Error("spanread does not read $__range_s")
	}
	ref := regexp.MustCompile(`\$\{?(\w+)`)
	for _, name := range rangeWordVars {
		kind, vs := variable(doc, name)
		query, _ := vs["query"].(string)
		if kind != "CustomVariable" || vs["hide"] != "hideVariable" {
			t.Errorf("variable %s is %s hide %v, want a hidden CustomVariable", name, cmp.Or(kind, "missing"), vs["hide"])
		}
		refs := ref.FindAllStringSubmatch(query, -1)
		for _, m := range refs {
			if m[1] != "spanread" {
				t.Errorf("variable %s query %q reads $%s, want it to depend only on spanread", name, query, m[1])
			}
		}
		if len(refs) == 0 {
			t.Errorf("variable %s query %q does not read spanread", name, query)
		}
	}
	lengths := []struct {
		rangeS int
		want   string
	}{
		{45, "45 seconds"},
		{119, "119 seconds"},
		{120, "2 minutes"},
		{150, "3 minutes"},
		{7199, "120 minutes"},
		{7200, "2 hours"},
		{19980, "5.6 hours"},
		{172799, "48 hours"},
		{172800, "2 days"},
		{216000, "2.5 days"},
		{280800, "3.3 days"},
		{2592000, "30 days"},
	}
	reads := make([]map[string]string, len(lengths))
	for i, l := range lengths {
		reads[i] = rangeWords(t, doc, queryResultRow(t, variableQuery(doc, "spanread"), l.rangeS))
	}
	none := rangeWords(t, doc, "")
	for _, name := range rangeWordVars {
		_, vs := variable(doc, name)
		if current, _ := vs["current"].(map[string]any); current["text"] != none[name] || current["value"] != none[name] {
			t.Errorf("variable %s saves %v, want %q, its text before spanread reads a length", name, current, none[name])
		}
	}
	render := func(title string, words map[string]string) string {
		fill := strings.NewReplacer("${span}", words["span"], "${spanunit}", words["spanunit"], "${spanthe}", words["spanthe"])
		return strings.Join(strings.Fields(fill.Replace(title)), " ")
	}
	panels := loadPanels(t)
	specs := panelSpecs(t)
	figures := 0
	for name, p := range panels {
		title := p.Spec.Title
		ranged := p.Spec.VizConfig.Group != "timeseries" && slices.ContainsFunc(queriesOf(specs, specs[name]), func(q any) bool {
			expr, _ := dig(q, "spec", "query", "spec", "expr").(string)
			return strings.Contains(expr, "$__range")
		})
		if ranged {
			figures++
		}
		if !ranged && !strings.Contains(title, "${span") {
			continue
		}
		fallback := render(title, none)
		if strings.Count(fallback, "the range") != 1 || strings.Contains(fallback, "the the") {
			t.Errorf("%s title %q reads %q before the length is read, want it to name \"the range\" once", name, title, fallback)
		}
		for i, l := range lengths {
			named := render(title, reads[i])
			length := l.want
			if strings.Contains(title, "${spanthe}") {
				length = "the " + l.want
			}
			if want := strings.Replace(fallback, "the range", length, 1); named != want {
				t.Errorf("%s title %q reads %q over %d s, want %q", name, title, named, l.rangeS, want)
				break
			}
		}
	}
	if figures == 0 {
		t.Error("no range figure found; the pattern no longer reads the dashboard")
	}
}

type shippedRule struct{ expr, forDur string }

// shippedRules reads the expr and the for of each alert in alerts/promql.yaml. A block
// expr is joined into one line, with no space inside its parentheses.
func shippedRules(t *testing.T) map[string]shippedRule {
	t.Helper()
	raw, err := os.ReadFile("alerts/promql.yaml")
	if err != nil {
		t.Fatalf("Setup: read rules: %v", err)
	}
	rules, name, blockIndent := map[string]shippedRule{}, "", -1
	for line := range strings.Lines(string(raw)) {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if blockIndent >= 0 && indent > blockIndent && trimmed != "" {
			r := rules[name]
			r.expr = strings.TrimSpace(r.expr + " " + trimmed)
			r.expr = strings.NewReplacer("( ", "(", " )", ")").Replace(r.expr)
			rules[name] = r
			continue
		}
		blockIndent = -1
		key, value, _ := strings.Cut(trimmed, ": ")
		switch key {
		case "- alert":
			name = value
		case "expr":
			r := rules[name]
			r.expr = value
			if value == "|" {
				r.expr, blockIndent = "", indent
			}
			rules[name] = r
		case "for":
			r := rules[name]
			r.forDur = value
			rules[name] = r
		}
	}
	return rules
}

var (
	regressedWindow = regexp.MustCompile(`max_over_time\(registrystats_image_pulls_total\[(\w+)\]\)`)
	degradedWindow  = regexp.MustCompile(`^increase\(registrystats_collect_errors_total\[(\w+)\]\) > 0 or \(`)
	degradedUptime  = regexp.MustCompile(`and on \(job, instance\) \(time\(\) - process_start_time_seconds < [^)]+\)`)
)

// nowTileSignals lists the tiles that mirror a shipped rule and the part of the rule each
// tile's query must hold. Only these tiles may keep a rule's fixed windows.
var nowTileSignals = []struct {
	tile, rule string
	want       func(r shippedRule) string
}{
	{"Collector not reporting", "RegistryStatsTargetDown", func(r shippedRule) string { return r.expr }},
	{"Collector not reporting", "RegistryStatsTargetAbsent", func(r shippedRule) string { return r.expr }},
	{"Checks stalled", "RegistryStatsCollectStalled", func(r shippedRule) string { return strings.TrimSuffix(r.expr, " == 0") }},
	{"Checks stalled", "RegistryStatsCollectStalled", func(r shippedRule) string { return "[" + heldWindow(r.forDur) + ":1m]" }},
	{"Checks failing", "RegistryStatsSourceDegraded", func(r shippedRule) string {
		w := "no window"
		if m := degradedWindow.FindStringSubmatch(r.expr); m != nil {
			w = m[1]
		}
		return `increase(registrystats_collect_errors_total{source=~"$registry"}[` + w + `]) > 0 or (`
	}},
	{"Checks failing", "RegistryStatsSourceDegraded", func(r shippedRule) string {
		uptime := "no uptime arm"
		if m := degradedUptime.FindStringSubmatch(r.expr); m != nil {
			uptime = m[0]
		}
		return uptime
	}},
	{"Counts that went down", "RegistryStatsPullCountRegressed", func(r shippedRule) string {
		w := "no window"
		if m := regressedWindow.FindStringSubmatch(r.expr); m != nil {
			w = m[1]
		}
		return "}[" + w + "]) - registrystats_image_pulls_total{"
	}},
}

// A tile for a condition that holds now reads its rule's own signal and windows, never the
// dashboard range, so the tile shows exactly while the rule's condition holds.
func TestDashboard_NowTilesReadTheirRuleSignal(t *testing.T) {
	rules := shippedRules(t)
	panels := loadPanels(t)
	for _, c := range nowTileSignals {
		if !strings.Contains(panelByTitle(t, panels, c.tile).Spec.Description, c.rule) {
			t.Errorf("%q description does not name %s, the rule it mirrors", c.tile, c.rule)
		}
		r, ok := rules[c.rule]
		if !ok || r.expr == "" {
			t.Errorf("alerts/promql.yaml has no single-line expr for %s", c.rule)
			continue
		}
		expr := panelByTitle(t, panels, c.tile).Spec.Data.Spec.Queries[0].Spec.Query.Spec.Expr
		if want := c.want(r); !strings.Contains(expr, want) {
			t.Errorf("%q does not read %s's signal %q: %.200s", c.tile, c.rule, want, expr)
		}
	}
}

// A rule-backed tile shows while its alert fires, and checks the rule's condition itself,
// held at every minute of the rule's for:, only where no ruler wrote that alert within the
// for:, so the tile and the alert agree and neither lights for a blip shorter than the for:.
func TestDashboard_RuleTilesShowWhileTheirAlertFires(t *testing.T) {
	rules := shippedRules(t)
	panels := loadPanels(t)
	for _, c := range nowTileSignals {
		r := rules[c.rule]
		expr := panelByTitle(t, panels, c.tile).Spec.Data.Spec.Queries[0].Spec.Query.Spec.Expr
		held := int(promDuration(r.forDur).Minutes())
		for _, want := range []string{
			`ALERTS{alertname="` + c.rule + `", alertstate="firing"`,
			"[" + heldWindow(r.forDur) + ":1m]) > " + strconv.Itoa(held),
			`unless on () `,
			`count(max_over_time(ALERTS{alertname="` + c.rule + `"}[` + r.forDur + `]))`,
		} {
			if held == 0 || !strings.Contains(expr, want) {
				t.Errorf("%q does not hold %s for its for: %q, want %q in %.200s", c.tile, c.rule, r.forDur, want, expr)
			}
		}
	}
}

var (
	fixedPeriod  = regexp.MustCompile(`\[((?:\d+(?:ms|[smhdwy]))+)(?::(?:\d+(?:ms|[smhdwy]))*)?\]|(offset -?\d+(?:ms|[smhdwy])|@ \d+(?:\.\d+)?)`)
	ruleWindow   = regexp.MustCompile(`\[((?:\d+(?:ms|[smhdwy]))+)[\]:]`)
	durationPart = regexp.MustCompile(`(\d+)(ms|[smhdwy])`)
	fixedTitle   = regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s*(?:[smhdwy]|mins?|minutes?|hours?|days?|weeks?|months?|years?)\b|` +
		`\b(?:hourly|daily|weekly|monthly|yearly|today|yesterday)\b|` +
		`\b(?:per|a|an|one|last|past|this|next) (?:second|minute|hour|day|week|fortnight|month|quarter|year)s?\b`)
)

// enclosingCall names the function whose parentheses hold expr[at], skipping quoted strings.
func enclosingCall(expr string, at int) string {
	depth, quoted := 0, false
	for i := at - 1; i >= 0; i-- {
		switch c := expr[i]; {
		case c == '"' && (i == 0 || expr[i-1] != '\\'):
			quoted = !quoted
		case quoted:
		case c == ')':
			depth++
		case c == '(' && depth > 0:
			depth--
		case c == '(':
			name := strings.TrimRight(expr[:i], " ")
			return name[strings.LastIndexFunc(name, func(r rune) bool {
				return r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
			})+1:]
		}
	}
	return ""
}

// heldWindow returns the subquery range that checks a condition at every minute of forDur.
// A subquery range is open at its start, so [3h:1m] holds 180 samples over 179 minutes and
// would light a tile a minute before its rule fires; one more minute holds 181 over 180.
func heldWindow(forDur string) string {
	d := promDuration(forDur) + time.Minute
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return strconv.Itoa(m) + "m"
	case m == 0:
		return strconv.Itoa(h) + "h"
	}
	return strconv.Itoa(h) + "h" + strconv.Itoa(m) + "m"
}

// promDuration reads a PromQL duration such as 1h30m.
func promDuration(s string) time.Duration {
	unit := map[string]time.Duration{
		"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour,
		"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "y": 365 * 24 * time.Hour,
	}
	var d time.Duration
	for _, m := range durationPart.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(m[1])
		d += time.Duration(n) * unit[m[2]]
	}
	return d
}

// A figure that counts, sums or compares over time measures the time picker's range, except
// a tile in nowTileSignals, which measures its rule's own windows and for: so it shows
// exactly while the alert's condition holds. Any other fixed window passes only as
// last_over_time reaching back at most a day to the newest sample, or a rate's smoothing
// interval of at most 5 minutes. No title names a fixed length.
func TestDashboard_PeriodsFollowTheTimePicker(t *testing.T) {
	rules := shippedRules(t)
	windows := 0
	for name, p := range loadPanels(t) {
		if m := fixedTitle.FindString(p.Spec.Title); m != "" {
			t.Errorf("%s title %q names the fixed length %q, want the span variables", name, p.Spec.Title, m)
		}
		ruled := map[string]bool{}
		for _, s := range nowTileSignals {
			if s.tile != p.Spec.Title {
				continue
			}
			for _, m := range ruleWindow.FindAllStringSubmatch(rules[s.rule].expr, -1) {
				ruled[m[1]] = true
			}
			if f := rules[s.rule].forDur; f != "" {
				ruled[f], ruled[heldWindow(f)] = true, true
			}
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			e := q.Spec.Query.Spec.Expr
			for _, m := range fixedPeriod.FindAllStringSubmatchIndex(e, -1) {
				windows++
				if m[4] >= 0 {
					t.Errorf("%s %q query %s pins %q, want offset $__range or @ start() or end()", name, p.Spec.Title, q.Spec.RefID, e[m[4]:m[5]])
					continue
				}
				call, w := enclosingCall(e, m[0]), e[m[2]:m[3]]
				switch d := promDuration(w); {
				case call == "last_over_time" && d <= 24*time.Hour:
				case (call == "rate" || call == "irate") && d <= 5*time.Minute:
				case ruled[w]:
				default:
					t.Errorf("%s %q query %s measures %s(...%s), want $__range or $__interval", name, p.Spec.Title, q.Spec.RefID, call, e[m[0]:m[1]])
				}
			}
		}
	}
	if windows == 0 {
		t.Error("no fixed window found; the pattern no longer reads the dashboard")
	}
}

func panelSpecs(t *testing.T) map[string]map[string]any {
	t.Helper()
	docSpec, _ := loadDoc(t)["spec"].(map[string]any)
	elements, _ := docSpec["elements"].(map[string]any)
	specs := map[string]map[string]any{}
	for name, el := range elements {
		em, _ := el.(map[string]any)
		specs[name], _ = em["spec"].(map[string]any)
	}
	return specs
}

func dig(v any, path ...string) any {
	for _, key := range path {
		m, _ := v.(map[string]any)
		v = m[key]
	}
	return v
}

// dropsNonFinite reports whether steps turn the fields into rows of their last value, keep
// only the finite non-null rows and turn those back into fields, so a stat never reads ∞ or NaN.
func dropsNonFinite(steps []any) bool {
	stage := 0
	for _, s := range steps {
		opts := dig(s, "spec", "options")
		switch group := dig(s, "group"); {
		case stage == 0 && group == "reduce" && dig(opts, "mode") == "seriesToRows":
			reducers, _ := dig(opts, "reducers").([]any)
			if slices.Equal(reducers, []any{"lastNotNull"}) {
				stage = 1
			}
		case stage == 1 && group == "filterByValue" && dig(opts, "type") == "include" && dig(opts, "match") == "all":
			filters, _ := dig(opts, "filters").([]any)
			notNull, finite := false, false
			for _, f := range filters {
				if dig(f, "fieldName") != "Last *" {
					continue
				}
				from, _ := dig(f, "config", "options", "from").(float64)
				to, _ := dig(f, "config", "options", "to").(float64)
				notNull = notNull || dig(f, "config", "id") == "isNotNull"
				finite = finite || dig(f, "config", "id") == "between" && from <= -1e300 && to >= 1e300
			}
			if notNull && finite {
				stage = 2
			}
		case stage == 2 && group == "rowsToFields":
			return true
		}
	}
	return false
}

func mapsNaNToDash(overrides []any, field string) bool {
	for _, o := range overrides {
		if dig(o, "matcher", "id") != "byName" || dig(o, "matcher", "options") != field {
			continue
		}
		props, _ := dig(o, "properties").([]any)
		for _, p := range props {
			mappings, _ := dig(p, "value").([]any)
			for _, m := range mappings {
				if dig(p, "id") == "mappings" && dig(m, "type") == "special" &&
					dig(m, "options", "match") == "null+nan" && dig(m, "options", "result", "text") == "-" {
					return true
				}
			}
		}
	}
	return false
}

var (
	frameValue = regexp.MustCompile(`^Value #(\w+)$`)
	aboveZero  = regexp.MustCompile(`\s>\s0(?:\s|\)|$)`)
)

// A percent Grafana computes divides by the earlier span, which can count 0 downloads or
// have no series at all, and Grafana prints those quotients as ∞ and NaN. So either the
// non-finite quotients are dropped before a stat reads them, or the divisor's query keeps
// only values above 0 and the missing divisor's NaN reads as a dash.
func TestDashboard_ComputedPercentsNeverShowInfinityOrNaN(t *testing.T) {
	divisions := 0
	for name, ps := range panelSpecs(t) {
		steps, _ := dig(ps, "data", "spec", "transformations").([]any)
		queries, _ := dig(ps, "data", "spec", "queries").([]any)
		overrides, _ := dig(ps, "vizConfig", "spec", "fieldConfig", "overrides").([]any)
		for i, s := range steps {
			opts := dig(s, "spec", "options")
			if dig(s, "group") != "calculateField" || dig(opts, "binary", "operator") != "/" {
				continue
			}
			divisions++
			alias, _ := dig(opts, "alias").(string)
			if dropsNonFinite(steps[i+1:]) {
				continue
			}
			right, _ := dig(opts, "binary", "right", "matcher", "options").(string)
			m := frameValue.FindStringSubmatch(right)
			guarded := false
			for _, q := range queries {
				expr, _ := dig(q, "spec", "query", "spec", "expr").(string)
				guarded = guarded || m != nil && dig(q, "spec", "refId") == m[1] && aboveZero.MatchString(expr)
			}
			if !guarded || !mapsNaNToDash(overrides, alias) {
				t.Errorf("%s %v: %s divides by %q, want its non-finite values dropped before display, "+
					"or a divisor query kept above 0 (%v) and a null+nan mapping to \"-\" (%v)",
					name, ps["title"], alias, right, guarded, mapsNaNToDash(overrides, alias))
			}
		}
	}
	if divisions == 0 {
		t.Error("no computed division found; the pattern no longer reads the dashboard")
	}
}

// summaryTiles are the Overview's always-visible figures, in their order on screen.
var summaryTiles = []string{
	"Downloads in ${span} ${spanunit}",
	"Change over ${spanthe} ${spanunit}",
	"Growth over ${spanthe} ${spanunit}",
}

// tabletGridPx is the width an Overview auto grid fills on a 1024 px tablet, and
// autoGridGapPx the gap between two tiles, both measured in Grafana 13.2.3.
const tabletGridPx, autoGridGapPx = 912, 8

// rowSpec returns the spec of the first RowsLayoutRow titled title.
func rowSpec(t *testing.T, title string) map[string]any {
	t.Helper()
	var found map[string]any
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if spec, _ := n["spec"].(map[string]any); n["kind"] == "RowsLayoutRow" && spec["title"] == title && found == nil {
				found = spec
			}
			for _, child := range n {
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(loadDoc(t))
	if found == nil {
		t.Fatalf("Setup: no row titled %q", title)
	}
	return found
}

// The Summary row is always on screen, so its tiles share one line wherever the grid holds
// three columns: one column per tile, all of them within the 912 px grid of a 1024 px tablet.
// A grid too narrow for three columns but wide enough for two shows 2 + 1, which happens on
// some widths below 1024 px and on some laptop widths with the menu docked; a phone stacks them.
func TestDashboard_SummaryTilesShareOneLine(t *testing.T) {
	grid, _ := dig(rowSpec(t, "Summary figures"), "layout").(map[string]any)
	if grid["kind"] != "AutoGridLayout" {
		t.Fatalf("Summary figures is a %v, want an AutoGridLayout", grid["kind"])
	}
	specs := panelSpecs(t)
	items, _ := dig(grid, "spec", "items").([]any)
	titles := []string{}
	for _, it := range items {
		name, _ := dig(it, "spec", "element", "name").(string)
		title, _ := specs[name]["title"].(string)
		titles = append(titles, title)
	}
	if !slices.Equal(titles, summaryTiles) {
		t.Errorf("Summary holds %q, want %q", titles, summaryTiles)
	}
	columns, _ := dig(grid, "spec", "maxColumnCount").(float64)
	width, _ := dig(grid, "spec", "columnWidth").(float64)
	if int(columns) != len(titles) {
		t.Errorf("Summary has %v columns for %d tiles, want one per tile", columns, len(titles))
	}
	if line := len(titles)*int(width) + (len(titles)-1)*autoGridGapPx; dig(grid, "spec", "columnWidthMode") != "custom" || line > tabletGridPx {
		t.Errorf("Summary's %d tiles of %v px (%v) take %d px, want at most %d, a tablet's grid",
			len(titles), width, dig(grid, "spec", "columnWidthMode"), line, tabletGridPx)
	}
}

// Folding Summary folds the figures and the Downloads chart under them, and nothing else.
func TestDashboard_SummaryFoldsFiguresThenDownloads(t *testing.T) {
	specs := panelSpecs(t)
	titles := []string{}
	for _, it := range rowItems(rowSpec(t, "Summary")) {
		name, _ := dig(it, "spec", "element", "name").(string)
		title, _ := specs[name]["title"].(string)
		titles = append(titles, title)
	}
	if want := append(slices.Clone(summaryTiles), "Downloads"); !slices.Equal(titles, want) {
		t.Errorf("Summary folds %q, want %q", titles, want)
	}
}

// readsPanel returns the id of the panel whose raw results spec's one query reads through
// Grafana's Dashboard data source, or 0 when it runs its own queries.
func readsPanel(spec map[string]any) float64 {
	queries, _ := dig(spec, "data", "spec", "queries").([]any)
	if len(queries) != 1 || dig(queries[0], "spec", "query", "datasource", "name") != "-- Dashboard --" ||
		dig(queries[0], "spec", "query", "spec", "withTransforms") != false {
		return 0
	}
	id, _ := dig(queries[0], "spec", "query", "spec", "panelId").(float64)
	return id
}

// queriesOf returns the queries whose results a panel reads: its own, or those of the panel
// it reads through the Dashboard data source.
func queriesOf(specs map[string]map[string]any, spec map[string]any) []any {
	if id := readsPanel(spec); id != 0 {
		for _, other := range specs {
			if other["id"] == id {
				spec = other
			}
		}
	}
	queries, _ := dig(spec, "data", "spec", "queries").([]any)
	return queries
}

// shownFields returns the fields a panel's transformations leave: each query's legend and each
// calculated field, less those the organize step excludes.
func shownFields(specs map[string]map[string]any, spec map[string]any) []string {
	var fields []string
	for _, q := range queriesOf(specs, spec) {
		legend, _ := dig(q, "spec", "query", "spec", "legendFormat").(string)
		fields = append(fields, legend)
	}
	steps, _ := dig(spec, "data", "spec", "transformations").([]any)
	excluded := map[string]any{}
	for _, s := range steps {
		switch dig(s, "group") {
		case "calculateField":
			alias, _ := dig(s, "spec", "options", "alias").(string)
			fields = append(fields, alias)
		case "organize":
			excluded, _ = dig(s, "spec", "options", "excludeByName").(map[string]any)
		}
	}
	return slices.DeleteFunc(fields, func(f string) bool { return excluded[f] == true })
}

// Change and Growth each show one value and no label inside the tile: Change the difference
// in downloads in the text colour, Growth the percent coloured by its thresholds. Growth reads
// Change's raw results, so the Overview's heaviest queries run once for both.
func TestDashboard_ChangeAndGrowthShowOneValueEach(t *testing.T) {
	all := panelSpecs(t)
	specs := map[string]map[string]any{}
	for _, spec := range all {
		title, _ := spec["title"].(string)
		specs[title] = spec
	}
	if change, growth := specs[summaryTiles[1]], specs[summaryTiles[2]]; change == nil || growth == nil ||
		readsPanel(change) != 0 || readsPanel(growth) != change["id"] {
		t.Errorf("Growth reads panel %v, want Change's id %v with withTransforms false", readsPanel(growth), change["id"])
	}
	for _, c := range []struct{ title, field, unit, colour string }{
		{summaryTiles[1], "Difference", "short", "fixed"},
		{summaryTiles[2], "Percent", "percentunit", "thresholds"},
	} {
		spec, ok := specs[c.title]
		if !ok {
			t.Errorf("no panel titled %q", c.title)
			continue
		}
		if got := shownFields(all, spec); !slices.Equal(got, []string{c.field}) {
			t.Errorf("%q shows %q, want only %s", c.title, got, c.field)
		}
		viz := dig(spec, "vizConfig", "spec")
		if mode := dig(viz, "options", "textMode"); mode != "value" {
			t.Errorf("%q textMode = %v, want value, so no field name shows in the tile", c.title, mode)
		}
		defaults := dig(viz, "fieldConfig", "defaults")
		if unit, colour := dig(defaults, "unit"), dig(defaults, "color", "mode"); unit != c.unit || colour != c.colour {
			t.Errorf("%q is unit %v, colour %v, want %s and %s", c.title, unit, colour, c.unit, c.colour)
		}
	}
}

// The short unit scales a download count to K and M, so a fixed 0 decimals would print
// 1,775 downloads as 2 K; download figures leave the decimals to the unit.
func TestDashboard_DownloadFiguresKeepTheirSignificantDigits(t *testing.T) {
	figures := 0
	for name, ps := range panelSpecs(t) {
		queries, _ := json.Marshal(dig(ps, "data", "spec", "queries"))
		if !strings.Contains(string(queries), "registrystats_image_pulls_total") {
			continue
		}
		figures++
		fc := dig(ps, "vizConfig", "spec", "fieldConfig")
		defaults, _ := dig(fc, "defaults").(map[string]any)
		if defaults["unit"] == "short" && defaults["decimals"] == 0.0 {
			t.Errorf("%s %v defaults to short with 0 decimals", name, ps["title"])
		}
		overrides, _ := dig(fc, "overrides").([]any)
		for _, o := range overrides {
			props, _ := dig(o, "properties").([]any)
			unit, decimals := defaults["unit"], defaults["decimals"]
			for _, p := range props {
				switch dig(p, "id") {
				case "unit":
					unit = dig(p, "value")
				case "decimals":
					decimals = dig(p, "value")
				}
			}
			if unit == "short" && decimals == 0.0 {
				t.Errorf("%s %v field %v is short with 0 decimals", name, ps["title"], dig(o, "matcher", "options"))
			}
		}
	}
	if figures == 0 {
		t.Error("no download figure found; the pattern no longer reads the dashboard")
	}
}

var (
	packageRanking = regexp.MustCompile(`\b(and|unless) on \(owner, repo\$\{split\}\) topk\((\d+),`)
	titleTopN      = regexp.MustCompile(`\btop (\d+)\b`)
)

// A chart of the most downloaded packages draws only them, with no series lumping the
// rest, and its title states how many it ranks, so no reader takes it for every package.
func TestDashboard_PackageChartsNameTheirTopN(t *testing.T) {
	charts := 0
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group != "timeseries" {
			continue
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			for _, m := range packageRanking.FindAllStringSubmatch(q.Spec.Query.Spec.Expr, -1) {
				charts++
				if m[1] == "unless" {
					t.Errorf("%s %q query %s draws the packages outside its top %s as one series", name, p.Spec.Title, q.Spec.RefID, m[2])
				}
				if tm := titleTopN.FindStringSubmatch(p.Spec.Title); tm == nil || tm[1] != m[2] {
					t.Errorf("%s title %q does not name the top %s its query %s ranks", name, p.Spec.Title, m[2], q.Spec.RefID)
				}
				// A range query's end() is its end aligned to the step, so it would rank another
				// window than Package downloads, whose instant query runs at the dashboard's end.
				if e := q.Spec.Query.Spec.Expr; strings.Contains(e, "@ end()") || !strings.Contains(e, "@ ${__to:date:seconds}") {
					t.Errorf("%s %q query %s ranks at %q, want @ ${__to:date:seconds}", name, p.Spec.Title, q.Spec.RefID, e)
				}
			}
		}
	}
	if charts == 0 {
		t.Error("no ranked package chart found; the pattern no longer reads the dashboard")
	}
}

var roundedChange = regexp.MustCompile(`(\w*)\(delta\(\(max by \(owner, repo, registry\) \(registrystats_image_pulls_total\{[^}]*\}\)\)\[\$__range:1m\]`)

// Package downloads lists every package's downloads and the Downloads tile states their
// total, so both round each package and registry before any sum: rounding after it
// would leave the rows a few downloads off the tile.
func TestDashboard_RangeTotalsAddUpFromRoundedPackages(t *testing.T) {
	figures := 0
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group == "timeseries" {
			continue
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			for _, m := range roundedChange.FindAllStringSubmatch(q.Spec.Query.Spec.Expr, -1) {
				figures++
				if m[1] != "round" {
					t.Errorf("%s %q query %s takes the range change inside %q, want round( around each package and registry",
						name, p.Spec.Title, q.Spec.RefID, m[1])
				}
			}
		}
	}
	if figures == 0 {
		t.Error("no range total found; the pattern no longer reads the dashboard")
	}
}

// Geometry of a Grafana 13.2.3 table, measured at every width: a grid unit is 30 px
// with 8 px between units, the table body is 58 px shorter than its grid box, the
// header row is 34 px, and a row is 36 px at cellHeight sm and 48 px at lg.
const (
	gridUnitPx, gridGapPx, tableChromePx, tableHeaderPx = 30, 8, 58, 34
)

var tableRowPx = map[string]int{"sm": 36, "lg": 48}

// gridHeights maps each element a GridLayoutItem places to its height in grid units.
func gridHeights(t *testing.T) map[string]int {
	t.Helper()
	heights := map[string]int{}
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if n["kind"] == "GridLayoutItem" {
				name, _ := dig(n, "spec", "element", "name").(string)
				h, _ := dig(n, "spec", "height").(float64)
				heights[name] = int(h)
			}
			for _, child := range n {
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(loadDoc(t))
	return heights
}

// A table whose query caps its rows shows all of them in its fixed height, so none
// hides behind a scroll on an install large enough to fill the cap.
func TestDashboard_CappedTablesShowEveryRow(t *testing.T) {
	heights := gridHeights(t)
	capped := 0
	for name, ps := range panelSpecs(t) {
		queries, _ := dig(ps, "data", "spec", "queries").([]any)
		if dig(ps, "vizConfig", "group") != "table" || len(queries) == 0 {
			continue
		}
		expr, _ := dig(queries[0], "spec", "query", "spec", "expr").(string)
		rows := 0
		for _, m := range rowCap.FindAllStringSubmatch(expr, -1) {
			n, _ := strconv.Atoi(m[1])
			rows += n
		}
		if rows == 0 {
			continue
		}
		capped++
		opts := dig(ps, "vizConfig", "spec", "options")
		cell, _ := dig(opts, "cellHeight").(string)
		rowPx, measured := tableRowPx[cell]
		if !measured {
			t.Errorf("%s %v has cellHeight %q, whose row height is not measured", name, ps["title"], cell)
			continue
		}
		body := heights[name]*(gridUnitPx+gridGapPx) - gridGapPx - tableChromePx
		if dig(opts, "showHeader") == true {
			body -= tableHeaderPx
		}
		if fit := body / rowPx; rows > fit {
			t.Errorf("%s %v caps %d rows but its %d grid units show %d", name, ps["title"], rows, heights[name], fit)
		}
	}
	if capped == 0 {
		t.Error("no capped table found; the pattern no longer reads the dashboard")
	}
}
