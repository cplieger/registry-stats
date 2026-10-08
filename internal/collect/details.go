package collect

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/internal/obs"
	"github.com/cplieger/registry-stats/internal/registry"
)

// detailMaxAge is how long a read count stays published before it is read
// again, and the span a cold cache is filled over.
const detailMaxAge = 24 * time.Hour

// Details keeps each detailed image's tag and version counts across cycles,
// refreshed after the cycle's pull counts are published. A count is read again
// when the image's push or update time moved or the read is detailMaxAge old;
// unread images go longest-waiting first and failed reads after them, at most
// as many per cycle as fills a cold cache within detailMaxAge. A cached count
// survives a cycle that did not read its image and is dropped only when a
// complete cycle no longer details it. Construct via NewDetails; one goroutine
// at a time.
type Details struct {
	logger     *slog.Logger
	now        func() time.Time
	cache      map[detailKey]*cachedDetail
	omitted    map[registry.ID]bool
	readers    []Source
	coldCycles int
}

type detailKey struct {
	ref    registry.RepoRef
	source registry.ID
}

type cachedDetail struct {
	read         time.Time
	stamp        time.Time
	missingSince time.Time
	failed       time.Time
	detail       registry.Detail
}

// NewDetails returns a Details that reads through sources, at most one per
// registry.ID. poll is the interval between cycles; 0 means one cycle only,
// which reads every never-read image at once.
func NewDetails(logger *slog.Logger, poll time.Duration, sources ...Source) *Details {
	coldCycles := 1
	if poll > 0 {
		// The cycles that start within detailMaxAge of the first one.
		coldCycles = max(1, int((detailMaxAge+poll-1)/poll))
	}
	return &Details{
		logger:     logger,
		now:        time.Now,
		cache:      make(map[detailKey]*cachedDetail),
		omitted:    make(map[registry.ID]bool),
		readers:    sources,
		coldCycles: coldCycles,
	}
}

// Refresh logs the onset of each source's detail-cap overflow, reads the
// counts that are due for cycle's detailed images, and returns every cached
// count of those images with each source's oldest read time.
func (d *Details) Refresh(ctx context.Context, cycle *Cycle) ([]obs.DetailMetric, []obs.DetailAge) {
	for _, s := range cycle.Sources {
		if s.Omitted > 0 && !d.omitted[s.Source] {
			d.logger.Warn("image details omitted", "source", s.Source.String(), "omitted", s.Omitted)
		}
		d.omitted[s.Source] = s.Omitted > 0
	}

	var (
		values []obs.DetailMetric
		ages   []obs.DetailAge
	)
	for _, reader := range d.readers {
		images := detailedImages(cycle.Images, reader.Source())
		if sourceComplete(cycle.Sources, reader.Source()) {
			d.evict(reader.Source(), images)
		}
		d.refreshSource(ctx, reader, images)
		read, oldest := d.cached(reader.Source(), images)
		values = append(values, read...)
		if !oldest.IsZero() {
			ages = append(ages, obs.DetailAge{Source: reader.Source(), Oldest: oldest})
		}
	}
	return values, ages
}

// detailImage is one detailed image as the detail reads see it: its name, and
// the time whose change makes a cached count stale.
type detailImage struct {
	stamp time.Time
	ref   registry.RepoRef
}

// detailedImages lists source's images inside the detail cap. The stamp is
// GHCR's last push or Docker Hub's repository update, whichever the image has.
func detailedImages(images []obs.ImageMetric, source registry.ID) []detailImage {
	var out []detailImage
	for i := range images {
		image := &images[i]
		if image.Registry == source && image.Detailed {
			out = append(out, detailImage{
				ref:   registry.RepoRef{Owner: image.Owner, Repo: image.Repo},
				stamp: cmp.Or(image.LastPushed, image.Updated),
			})
		}
	}
	return out
}

// cached returns the read counts of one source's images and the oldest read
// time among them, zero when none has been read.
func (d *Details) cached(source registry.ID, images []detailImage) (values []obs.DetailMetric, oldest time.Time) {
	for _, image := range images {
		c := d.cache[detailKey{source: source, ref: image.ref}]
		if c.read.IsZero() {
			continue
		}
		values = append(values, obs.DetailMetric{Registry: source, Owner: image.ref.Owner, Repo: image.ref.Repo, Detail: c.detail})
		if oldest.IsZero() || c.read.Before(oldest) {
			oldest = c.read
		}
	}
	return values, oldest
}

// sourceComplete reports whether the cycle read source completely, so an image
// it did not measure is really gone or past the cap rather than unread.
func sourceComplete(sources []obs.SourceCycle, source registry.ID) bool {
	for _, s := range sources {
		if s.Source == source {
			return s.Complete
		}
	}
	return false
}

// evict drops the cached counts of source's images that are not in images.
func (d *Details) evict(source registry.ID, images []detailImage) {
	keep := make(map[detailKey]bool, len(images))
	for _, image := range images {
		keep[detailKey{source: source, ref: image.ref}] = true
	}
	for key := range d.cache {
		if key.source == source && !keep[key] {
			delete(d.cache, key)
		}
	}
}

// refreshSource reads one source's due counts until the cycle ends, the list
// runs out or the source rate-limits.
func (d *Details) refreshSource(ctx context.Context, reader Source, images []detailImage) {
	source := reader.Source()
	for _, image := range d.due(source, images) {
		if ctx.Err() != nil {
			return
		}
		detail, err := reader.ReadDetail(ctx, image.ref)
		if ctx.Err() != nil {
			return
		}
		c := d.cache[detailKey{source: source, ref: image.ref}]
		if err != nil {
			if errors.Is(err, httpx.ErrRateLimited) {
				d.logger.Warn("detail reads stopped", "source", source.String(), "reason", "rate_limited")
				return
			}
			// A failed read unpublishes the count rather than keep one that may be wrong.
			c.read, c.detail, c.failed = time.Time{}, registry.Detail{}, d.now()
			continue
		}
		c.detail, c.read, c.stamp, c.failed = detail, d.now(), image.stamp, time.Time{}
	}
}

// due returns the images to read this cycle: every changed or stale count,
// oldest read first, then the unread images within the cold-start budget,
// longest-waiting first and failed reads last, oldest failure first, so an
// image that keeps failing cannot hold the budget.
func (d *Details) due(source registry.ID, images []detailImage) []detailImage {
	now := d.now()
	var refresh, missing []detailImage
	for _, image := range images {
		key := detailKey{source: source, ref: image.ref}
		c, ok := d.cache[key]
		if !ok {
			c = &cachedDetail{missingSince: now}
			d.cache[key] = c
		}
		switch {
		case c.read.IsZero():
			missing = append(missing, image)
		case !image.stamp.IsZero() && !image.stamp.Equal(c.stamp), now.Sub(c.read) >= detailMaxAge:
			refresh = append(refresh, image)
		}
	}
	at := func(image detailImage) *cachedDetail { return d.cache[detailKey{source: source, ref: image.ref}] }
	slices.SortStableFunc(refresh, func(a, b detailImage) int { return at(a).read.Compare(at(b).read) })
	slices.SortStableFunc(missing, func(a, b detailImage) int {
		ca, cb := at(a), at(b)
		return cmp.Or(ca.failed.Compare(cb.failed), ca.missingSince.Compare(cb.missingSince), compareNames(a.ref, b.ref))
	})
	budget := (len(images) + d.coldCycles - 1) / d.coldCycles
	return append(refresh, missing[:min(len(missing), budget)]...)
}
