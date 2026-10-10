package main

import (
	"cmp"
	"encoding/json"
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
	Properties []struct {
		ID    string          `json:"id"`
		Value json.RawMessage `json:"value"`
	} `json:"properties"`
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

// topLevelArms splits expr at every " or " outside parentheses and quotes.
func topLevelArms(expr string) []string {
	var arms []string
	depth, start, quoted := 0, 0, false
	for i := range len(expr) {
		switch c := expr[i]; {
		case c == '"' && (i == 0 || expr[i-1] != '\\'):
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(expr[i:], " or "):
			arms = append(arms, expr[start:i])
			start = i + len(" or ")
		}
	}
	return append(arms, expr[start:])
}

// collectorJoin appends the collector to the status of every Registries out of step row.
const collectorJoin = `, "status", " · ", "status", "instance")`

// statusChain returns the or-chain the collector join wraps, or "" when no
// label_join wraps a parenthesized chain with collectorJoin.
func statusChain(expr string) string {
	const join = "label_join(("
	at := strings.Index(expr, join)
	if at < 0 {
		return ""
	}
	rest := expr[at+len(join)-1:]
	depth, quoted := 0, false
	for i := range len(rest) {
		switch c := rest[i]; {
		case c == '"' && (i == 0 || rest[i-1] != '\\'):
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				if !strings.HasPrefix(rest[i+1:], collectorJoin) {
					return ""
				}
				return rest[1:i]
			}
		}
	}
	return ""
}

// Every arm of Registries out of step names its row through a status label, and
// the status names the collector after it, so a row never reaches the table
// without the reason it is there or the collector that read it.
func TestDashboard_OutOfStepArmsEachCarryAStatus(t *testing.T) {
	p := panelByTitle(t, loadPanels(t), "Registries out of step")
	if len(p.Spec.Data.Spec.Queries) != 1 {
		t.Fatalf("Registries out of step has %d queries, want 1", len(p.Spec.Data.Spec.Queries))
	}
	expr := p.Spec.Data.Spec.Queries[0].Spec.Query.Spec.Expr
	chain := statusChain(expr)
	if chain == "" {
		t.Fatalf("Registries out of step does not append the collector to the status through label_join(..%s: %.160s", collectorJoin, expr)
	}
	arms := topLevelArms(chain)
	if len(arms) != 5 {
		t.Errorf("Registries out of step has %d arms, want 5 (two missing-package arms and three Unknown arms)", len(arms))
	}
	status := regexp.MustCompile(`^\(label_replace\(.*, "status", "[^"]+", "", ""\)`)
	for i, arm := range arms {
		if !status.MatchString(arm) {
			t.Errorf("Registries out of step arm %d does not set a status label through label_replace: %.120s", i+1, arm)
		}
	}
}

var byClause = regexp.MustCompile(`\b(\w+) by \(([^)]*)\)`)

// perRegistryByDesign are panels whose subject is a registry's own number, so
// they keep the registry whatever Split by registry says.
var perRegistryByDesign = map[string]string{
	"Counts that went down in ${span} ${spanunit}": "a drop is one registry restating its total",
}

// Every downloads query groups an image by owner and repo, and adds the registry
// only through the Split by registry switch.
func TestDashboard_DownloadGroupingsFollowTheSplitSwitch(t *testing.T) {
	groupings := 0
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
	doc := loadDoc(t)
	rules := map[string]any{}
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if n["kind"] == "AutoGridLayoutItem" {
				spec, _ := n["spec"].(map[string]any)
				element, _ := spec["element"].(map[string]any)
				name, _ := element["name"].(string)
				rules[name] = spec["conditionalRendering"]
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
	walk(doc)
	return rules
}

func showsOnlyWithData(rule any) bool {
	group, _ := rule.(map[string]any)
	spec, _ := group["spec"].(map[string]any)
	items, _ := spec["items"].([]any)
	if group["kind"] != "ConditionalRenderingGroup" || spec["visibility"] != "show" || len(items) == 0 {
		return false
	}
	for _, it := range items {
		item, _ := it.(map[string]any)
		itemSpec, _ := item["spec"].(map[string]any)
		if item["kind"] != "ConditionalRenderingData" || itemSpec["value"] != true {
			return false
		}
	}
	return true
}

var badCaseFilter = regexp.MustCompile(`\s(?:[<>]=?|[!=]=)\s+-?\d|\bunless\b`)

// An attention tile (a stat painted on a coloured background) is hidden until its
// condition holds: Grafana honours a "has data" rule only on an auto-grid item, and
// a query that returns a 0 for the healthy case would keep the tile on screen.
func TestDashboard_AttentionTilesShowOnlyWhileTheirConditionHolds(t *testing.T) {
	rules := autoGridRules(t)
	tiles := 0
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group != "stat" || p.Spec.VizConfig.Spec.Options.ColorMode != "background" {
			continue
		}
		tiles++
		rule, placed := rules[name]
		if !placed {
			t.Errorf("%s %q is not placed by an AutoGridLayoutItem, so no rule can hide it", name, p.Spec.Title)
		} else if !showsOnlyWithData(rule) {
			t.Errorf("%s %q conditionalRendering = %v, want a show group of has-data rules", name, p.Spec.Title, rule)
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			if e := q.Spec.Query.Spec.Expr; !badCaseFilter.MatchString(e) {
				t.Errorf("%s %q query %s returns data in the healthy case too: %.160s", name, p.Spec.Title, q.Spec.RefID, e)
			}
		}
	}
	if tiles == 0 {
		t.Error("no attention tile found; the pattern no longer reads the dashboard")
	}
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

// Hidden attention tiles live in a row that a hidden variable shows, because an empty
// row still draws its header. The variable counts every tile's own query and nothing
// else, so the row shows exactly while one of its tiles has data.
func TestDashboard_AttentionRowShowsWhileAnyTileHasData(t *testing.T) {
	doc := loadDoc(t)
	rows := rowsPlacing(t, doc)
	tiles := 0
	arms := map[string]int{}
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group != "stat" || p.Spec.VizConfig.Spec.Options.ColorMode != "background" {
			continue
		}
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
	if tiles == 0 {
		t.Error("no attention tile found; the pattern no longer reads the dashboard")
	}
	for variable, want := range arms {
		if got := strings.Count(variableQuery(doc, variable), `, "tile", "`); got != want {
			t.Errorf("variable %s counts %d arms, want %d, one per tile query", variable, got, want)
		}
	}
}

const spanTitle = "${span} ${spanunit}"

// A figure over the dashboard time range reads only with the range's length, so every
// panel outside the charts whose query reads $__range names it through the span
// variables. They follow the time picker, and their saved values make a title read
// "the range" while their query cannot run, so a title's own "the" is spanthe, which
// saves none. Grafana prints a query_result row as "{labels} value timestamp", which
// each variable's regex takes apart.
func TestDashboard_RangeFiguresNameTheirSpan(t *testing.T) {
	doc := loadDoc(t)
	for _, v := range []struct{ name, saved, row, fromRow string }{
		{"span", "the", `{u="days"} 2.6 1791585738061`, "2.6"},
		{"spanunit", "range", `{u="days"} 2.6 1791585738061`, "days"},
		{"spanthe", "", `{w="the"} 2592000 1791585738061`, "the"},
	} {
		kind, vs := variable(doc, v.name)
		if kind != "QueryVariable" || vs["refresh"] != "onTimeRangeChanged" || vs["hide"] != "hideVariable" {
			t.Errorf("variable %s is %s refresh %v hide %v, want a hidden QueryVariable refreshed on time range change",
				v.name, cmp.Or(kind, "missing"), vs["refresh"], vs["hide"])
			continue
		}
		if q := variableQuery(doc, v.name); !strings.Contains(q, "$__range_s") {
			t.Errorf("variable %s query %.120s does not read $__range_s", v.name, q)
		}
		current, _ := vs["current"].(map[string]any)
		if current["text"] != v.saved || current["value"] != v.saved {
			t.Errorf("variable %s saves %v, want %q so a title reads the range while the query fails", v.name, current, v.saved)
		}
		pattern, _ := vs["regex"].(string)
		re, err := regexp.Compile(strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/"))
		if err != nil {
			t.Errorf("variable %s regex %q: %v", v.name, pattern, err)
			continue
		}
		if m := re.FindStringSubmatch(v.row); len(m) != 2 || m[1] != v.fromRow {
			t.Errorf("variable %s regex %q takes %v from %s, want %q", v.name, pattern, m, v.row, v.fromRow)
		}
	}
	figures := 0
	for name, p := range loadPanels(t) {
		title := p.Spec.Title
		if strings.Contains(title, "${span}") && !strings.Contains(title, spanTitle) {
			t.Errorf("%s title %q names ${span} without its unit, want %q", name, title, spanTitle)
		}
		if strings.Contains(strings.ToLower(title), "the "+spanTitle) {
			t.Errorf("%s title %q reads \"the the range\" while the span query fails, want ${spanthe} before %s", name, title, spanTitle)
		}
		if p.Spec.VizConfig.Group == "timeseries" {
			continue
		}
		for _, q := range p.Spec.Data.Spec.Queries {
			if strings.Contains(q.Spec.Query.Spec.Expr, "$__range") {
				figures++
				if !strings.Contains(title, spanTitle) {
					t.Errorf("%s %q query %s reads $__range, want the title to name %s", name, title, q.Spec.RefID, spanTitle)
				}
				break
			}
		}
	}
	if figures == 0 {
		t.Error("no range figure found; the pattern no longer reads the dashboard")
	}
}

type shippedRule struct{ expr, forDur string }

// shippedRules reads the single-line expr and the for of each alert in alerts/promql.yaml.
func shippedRules(t *testing.T) map[string]shippedRule {
	t.Helper()
	raw, err := os.ReadFile("alerts/promql.yaml")
	if err != nil {
		t.Fatalf("Setup: read rules: %v", err)
	}
	rules, name := map[string]shippedRule{}, ""
	for line := range strings.Lines(string(raw)) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ": ")
		switch key {
		case "- alert":
			name = value
		case "expr":
			r := rules[name]
			r.expr = value
			rules[name] = r
		case "for":
			r := rules[name]
			r.forDur = value
			rules[name] = r
		}
	}
	return rules
}

var regressedWindow = regexp.MustCompile(`max_over_time\(registrystats_image_pulls_total\[(\w+)\]\)`)

// nowTileSignals lists the tiles that mirror a shipped rule and the part of the rule each
// tile's query must hold. Only these tiles may keep a rule's fixed windows.
var nowTileSignals = []struct {
	tile, rule string
	want       func(r shippedRule) string
}{
	{"Collector not reporting", "RegistryStatsTargetDown", func(r shippedRule) string { return r.expr }},
	{"Collector not reporting", "RegistryStatsTargetAbsent", func(r shippedRule) string { return r.expr }},
	{"Checks stalled", "RegistryStatsCollectStalled", func(r shippedRule) string { return strings.TrimSuffix(r.expr, " == 0") }},
	{"Checks stalled", "RegistryStatsCollectStalled", func(r shippedRule) string { return "[" + r.forDur + ":1m]" }},
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

var (
	fixedPeriod  = regexp.MustCompile(`\[((?:\d+(?:ms|[smhdwy]))+)(?::(?:\d+(?:ms|[smhdwy]))*)?\]|(offset -?\d+(?:ms|[smhdwy])|@ \d+(?:\.\d+)?)`)
	ruleWindow   = regexp.MustCompile(`\[((?:\d+(?:ms|[smhdwy]))+)[\]:]`)
	durationPart = regexp.MustCompile(`(\d+)(ms|[smhdwy])`)
	fixedTitle   = regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s*(?:[smhdwy]|mins?|minutes?|hours?|days?|weeks?|months?|years?)\b|` +
		`\b(?:hourly|daily|weekly|monthly|yearly|today|yesterday)\b|` +
		`\b(?:per|a|an|one|last|past|this|next) (?:second|minute|hour|day|week|fortnight|month|quarter|year)s?\b`)
)

// justifiedWindows holds each fixed window kept for a reason the rule's cases do not cover,
// by panel. The panel's description states the length, so a reader knows the time picker
// does not set it there.
var justifiedWindows = map[string]struct{ window, stated string }{
	"panel-26": {"7d", "7 days"},
}

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
// last_over_time reaching back at most a day to the newest sample, a rate's smoothing
// interval of at most 5 minutes, or an entry of justifiedWindows. No title names a fixed
// length.
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
				ruled[f] = true
			}
		}
		justified, isJustified := justifiedWindows[name]
		if isJustified && !strings.Contains(p.Spec.Description, justified.stated) {
			t.Errorf("%s %q keeps a fixed %s, want its description to state %q", name, p.Spec.Title, justified.window, justified.stated)
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
				case ruled[w], isJustified && justified.window == w:
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

// On a large install the packages outside the top five outweigh them many times over, so
// their one series sits unstacked on its own axis, leaving the five bars the left axis.
func TestDashboard_OtherPackagesKeepTheirOwnAxis(t *testing.T) {
	ps := panelSpecs(t)["panel-3"]
	queries, _ := dig(ps, "data", "spec", "queries").([]any)
	others := ""
	for _, q := range queries {
		if legend, _ := dig(q, "spec", "query", "spec", "legendFormat").(string); strings.HasPrefix(legend, "Other packages") {
			others, _ = dig(q, "spec", "refId").(string)
		}
	}
	if others == "" {
		t.Fatalf("panel-3 %v has no Other packages query", ps["title"])
	}
	overrides, _ := dig(ps, "vizConfig", "spec", "fieldConfig", "overrides").([]any)
	got := map[string]any{}
	for _, o := range overrides {
		if dig(o, "matcher", "id") == "byFrameRefID" && dig(o, "matcher", "options") == others {
			props, _ := dig(o, "properties").([]any)
			for _, p := range props {
				id, _ := dig(p, "id").(string)
				got[id] = dig(p, "value")
			}
		}
	}
	if got["custom.axisPlacement"] != "right" || dig(got["custom.stacking"], "mode") != "none" {
		t.Errorf("Other packages (query %s) axisPlacement %v stacking %v, want right and mode none",
			others, got["custom.axisPlacement"], got["custom.stacking"])
	}
	// A range of about an hour holds one bucket, and a one-point line draws nothing but its point.
	size, _ := got["custom.pointSize"].(float64)
	if got["custom.drawStyle"] == "line" && (got["custom.showPoints"] == "never" || size < 5) {
		t.Errorf("Other packages (query %s) is a line with showPoints %v and pointSize %v, want points of at least 5 px",
			others, got["custom.showPoints"], got["custom.pointSize"])
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
