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

type dashPanel struct {
	Spec struct {
		VizConfig struct {
			Spec struct {
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
				Queries []dashQuery `json:"queries"`
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

// Every arm of Registries out of step names its row through a status label, so a
// row never reaches the table without the reason it is there.
func TestDashboard_OutOfStepArmsEachCarryAStatus(t *testing.T) {
	p := panelByTitle(t, loadPanels(t), "Registries out of step")
	if len(p.Spec.Data.Spec.Queries) != 1 {
		t.Fatalf("Registries out of step has %d queries, want 1", len(p.Spec.Data.Spec.Queries))
	}
	arms := topLevelArms(p.Spec.Data.Spec.Queries[0].Spec.Query.Spec.Expr)
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
	"Counts that went down":             "a drop is one registry restating its total",
	"GHCR against Docker Hub per image": "compares the two registries by construction",
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
