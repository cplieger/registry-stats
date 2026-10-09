package main

import (
	"cmp"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
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
					EnablePagination bool `json:"enablePagination"`
				} `json:"options"`
				FieldConfig struct {
					Defaults struct {
						Min *float64 `json:"min"`
					} `json:"defaults"`
					Overrides []dashOverride `json:"overrides"`
				} `json:"fieldConfig"`
			} `json:"spec"`
		} `json:"vizConfig"`
		Title string `json:"title"`
		Data  struct {
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
	"Counts that went down in this range": "a drop is one registry restating its total",
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
	rangeBefore  = regexp.MustCompile(`\[\$__range:[^\]]+\] offset \$__range\)`)
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
// reported in the day before the range before began.
func TestDashboard_RangeComparisonsWaitForAWholeEarlierRange(t *testing.T) {
	comparisons := 0
	for name, p := range loadPanels(t) {
		for _, q := range p.Spec.Data.Spec.Queries {
			e := q.Spec.Query.Spec.Expr
			if !rangeBefore.MatchString(e) {
				continue
			}
			comparisons++
			if !earlierGuard.MatchString(e) {
				t.Errorf("%s %q query %s compares with the range before without checking that the data reaches back to its start: %.160s",
					name, p.Spec.Title, q.Spec.RefID, e)
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
// end, so a legend total over them disagrees with Downloads in this range.
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

var rowCap = regexp.MustCompile(`\b(?:topk|bottomk)\(\d+,`)

// A panel's height is fixed, so a table whose row count follows the range or Split
// by registry hides rows behind a scroll and cuts the one at its edge. Grafana sizes
// a page to whole rows, so every table whose query does not cap its rows pages.
func TestDashboard_UncappedTablesPageWholeRows(t *testing.T) {
	uncapped := 0
	for name, p := range loadPanels(t) {
		if p.Spec.VizConfig.Group != "table" || len(p.Spec.Data.Spec.Queries) == 0 {
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
