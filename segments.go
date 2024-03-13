package lsvd

import (
	"slices"
	"sort"
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/lab47/lsvd/logger"
	"github.com/lab47/mode"
	"github.com/oklog/ulid/v2"
)

type Segments struct {
	segmentsMu sync.Mutex
	segments   map[SegmentId]*Segment
}

func NewSegments() *Segments {
	return &Segments{
		segments: make(map[SegmentId]*Segment),
	}
}

func (s *Segments) SegmentIds() []SegmentId {
	var ret []SegmentId

	for k := range s.segments {
		ret = append(ret, k)
	}

	return ret
}

func (s *Segments) LiveSegments() []SegmentId {
	var ret []SegmentId

	for k, s := range s.segments {
		if s.deleted {
			continue
		}
		ret = append(ret, k)
	}

	return ret

}

func (s *Segments) Usage() float64 {
	s.segmentsMu.Lock()
	defer s.segmentsMu.Unlock()

	var used, size uint64

	for _, s := range s.segments {
		used += s.Used
		size += s.Size
	}

	return 100.0 * (float64(used) / float64(size)) // report as a percent
}

func (s *Segments) Create(segId SegmentId, stats *SegmentStats) {
	s.segmentsMu.Lock()
	defer s.segmentsMu.Unlock()

	s.segments[segId] = &Segment{
		Size: stats.Blocks,
		Used: stats.Blocks,
	}
}

func (s *Segments) CreateOrUpdate(segId SegmentId, usedBytes, usedBlocks uint64) {
	s.segmentsMu.Lock()
	defer s.segmentsMu.Unlock()

	// TODO where is the Size coming from in the create case??
	seg, ok := s.segments[segId]
	if ok {
		seg.UsedBytes += usedBytes
		seg.Used += usedBlocks
	} else {
		s.segments[segId] = &Segment{
			UsedBytes: usedBytes,
			Used:      usedBlocks,
		}
	}
}

func (s *Segments) UpdateUsage(log logger.Logger, self SegmentId, affected []PartialExtent) {
	s.segmentsMu.Lock()
	defer s.segmentsMu.Unlock()

	warnedSegments := map[SegmentId]struct{}{}

	for _, r := range affected {
		rng := r.Live

		if seg, ok := s.segments[r.Segment]; ok {
			if seg.deleted {
				continue
			}

			// If we've affected ourselves, that's fine, but we can't
			// run the detectedCleared logic because it will misfire since
			// we might have the same extent written multiple times in the same
			// segment, that's totally valid.
			if r.Segment != self && mode.Debug() {
				if o, ok := seg.detectedCleared(rng); ok {
					log.Warn("detected clearing overlapping extent", "orig", o, "cur", r)
				}
				seg.cleared = append(seg.cleared, rng)
			}

			seg.Used -= uint64(rng.Blocks)
		} else {
			if _, seen := warnedSegments[r.Segment]; !seen {
				log.Warn("missing segment during usage update", "id", r.Segment.String())
				warnedSegments[r.Segment] = struct{}{}
			}
		}
	}
}

func (s *Segments) LogSegmentInfo(log hclog.Logger) {
	s.segmentsMu.Lock()
	defer s.segmentsMu.Unlock()

	type ent struct {
		seg     SegmentId
		density float64
		stats   *Segment
	}

	var entries []ent

	for segId, stats := range s.segments {
		entries = append(entries, ent{segId, stats.Density(), stats})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].density < entries[j].density
	})

	for _, e := range entries {
		log.Info("segment density", "segment", e.seg,
			"density", e.density,
			"total", e.stats.Size,
			"used", e.stats.Used,
		)
	}
}

func (s *Segments) SetDeleted(segId SegmentId) {
	s.segmentsMu.Lock()
	defer s.segmentsMu.Unlock()

	s.segments[segId].deleted = true
}

func (s *Segments) FindDeleted() []SegmentId {
	s.segmentsMu.Lock()
	defer s.segmentsMu.Unlock()

	var toDelete []SegmentId

	for i, s := range s.segments {
		if s.deleted {
			toDelete = append(toDelete, i)
		}
	}

	for _, i := range toDelete {
		delete(s.segments, i)
	}

	return toDelete
}

func (d *Segments) AllDeadSegments() ([]SegmentId, error) {
	d.segmentsMu.Lock()
	defer d.segmentsMu.Unlock()

	var ret []SegmentId

	for segId, stats := range d.segments {
		if stats.deleted {
			continue
		}

		if stats.Used == 0 {
			ret = append(ret, segId)
		}
	}

	return ret, nil
}

func (d *Segments) sortedSegments() []SegmentId {
	var ret []SegmentId

	for segId := range d.segments {
		ret = append(ret, segId)
	}

	slices.SortFunc(ret, func(a, b SegmentId) int {
		return ulid.ULID(a).Compare(ulid.ULID(b))
	})

	return ret
}

func (d *Segments) PickSegmentToGC(log logger.Logger, min float64, skip []SegmentId) (SegmentId, bool, error) {
	d.segmentsMu.Lock()
	defer d.segmentsMu.Unlock()

	var (
		smallestId    SegmentId
		smallestStats *Segment
	)

	for _, segId := range d.sortedSegments() {
		stats := d.segments[segId]

		if stats.deleted {
			continue
		}

		if slices.Contains(skip, segId) {
			continue
		}

		d := stats.Density()

		log.Trace("segment density", "segment", segId, "density", d)

		if d > min {
			continue
		}

		if smallestStats == nil || d < smallestStats.Density() {
			smallestStats = stats
			smallestId = segId
		}
	}

	if smallestStats == nil {
		return SegmentId{}, false, nil
	}

	return smallestId, true, nil
}
