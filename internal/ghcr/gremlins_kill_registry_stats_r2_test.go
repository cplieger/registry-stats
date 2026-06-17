package ghcr

// Tests added by mutant-killing unit registry-stats-r2. Round 2 also
// landed two production improvements in this package whose mutants are
// killed by the edit itself (no test needed):
//   - client.go: removed the provably-dead `if pacingJitter > 0` guard
//     (the preceding `<= 0` default makes pacingJitter always positive),
//     which deletes the client.go:125 BOUNDARY+NEGATION mutants.
//   - scraper.go: replaced the hand-rolled `if len(rest) > maxTitleDistance`
//     truncation with `rest[:min(len(rest), maxTitleDistance)]`, which
//     deletes the scraper.go:170 BOUNDARY mutant.
//
// The one test below covers the surviving mutant that the guard removal
// turned from dormant into killable. Helpers/identifiers defined here are
// prefixed gk_rstats_r2_; in-package helpers from scraper_test.go
// (shortRetry) are reused directly.

import (
	"context"
	"net/http"
	"testing"

	"github.com/cplieger/registry-stats/internal/model"
	"github.com/cplieger/registry-stats/internal/testsupport"
)

// --- client.go:121:19 CONDITIONALS_BOUNDARY (`pacingJitter <= 0`) ---

func TestGkRstatsR2_PacingJitterDefaultGuardsRandArgument(t *testing.T) {
	// After the dead `if pacingJitter > 0` guard was removed, the
	// `if pacingJitter <= 0 { pacingJitter = DefaultPacingJitter }` default
	// is load-bearing: it guarantees rand.Int64N(int64(pacingJitter)) gets
	// a positive argument. The boundary mutant (`<=` -> `<`) lets a
	// zero-value Options{} (PacingJitter == 0) slip through with
	// pacingJitter still 0, so rand.Int64N(0) panics.
	//
	// The rand call runs once per package, BEFORE the pacing timer select,
	// so we use an already-cancelled context: the non-mutant run computes
	// the (positive) jitter, then the select returns immediately via
	// ctx.Done() instead of waiting the 2-5s production pacing. Under the
	// mutant, rand.Int64N(0) panics before the select is reached, failing
	// the test. An explicit ref needs no HTTP, so http.DefaultClient is
	// never used.
	c := NewClient(http.DefaultClient, shortRetry(), Options{}, testsupport.QuietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	refs := []model.RepoRef{{Owner: "owner", Repo: "pkg1"}}
	entries, attempted, healthy := c.Collect(ctx, refs)

	// Cancelled before any scrape: nothing collected or attempted, and the
	// no-package-failure default keeps healthy true. The real kill signal
	// is the absence of a rand.Int64N(0) panic above.
	if len(entries) != 0 || attempted != 0 || !healthy {
		t.Errorf("Collect(cancelled, Options{}) = (entries=%d, attempted=%d, healthy=%v), want (0, 0, true)",
			len(entries), attempted, healthy)
	}
}
