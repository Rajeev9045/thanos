// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package compact

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/oklog/ulid"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/extprom"
	"github.com/thanos-io/thanos/pkg/objstore"
	"github.com/thanos-io/thanos/pkg/objstore/objtesting"
	"github.com/thanos-io/thanos/pkg/testutil"
	"github.com/thanos-io/thanos/pkg/testutil/e2eutil"
)

func TestGroup_Compact_e2e(t *testing.T) {
	objtesting.ForeachStore(t, func(t *testing.T, bkt objstore.Bucket) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		// Create fresh, empty directory for actual test.
		dir, err := ioutil.TempDir("", "test-compact")
		testutil.Ok(t, err)
		defer func() { testutil.Ok(t, os.RemoveAll(dir)) }()

		logger := log.NewLogfmtLogger(os.Stderr)

		ignoreDeletionMarkFilter := block.NewIgnoreDeletionMarkFilter(logger, objstore.WithNoopInstr(bkt), 48*time.Hour)
		duplicateBlocksFilter := block.NewDeduplicateFilter()
		metaFetcher, err := block.NewMetaFetcher(nil, 32, objstore.WithNoopInstr(bkt), "", nil, []block.MetadataFilter{
			ignoreDeletionMarkFilter,
			duplicateBlocksFilter,
		}, nil)
		testutil.Ok(t, err)

		reg := extprom.NewMockedRegisterer()
		gc := NewGarbage(logger, nil, metadata.NewDeletionMarker(reg, logger, objstore.WithNoopInstr(bkt)))
		markedForDeletion := reg.Collectors[0].(*prometheus.CounterVec)

		sy, err := NewSyncer(
			logger,
			nil,
			bkt,
			metaFetcher,
			1,
			false,
			false,
			gc,
			ignoreDeletionMarkFilter,
			duplicateBlocksFilter,
		)
		testutil.Ok(t, err)

		comp, err := tsdb.NewLeveledCompactor(ctx, reg, logger, []int64{1000, 3000}, nil)
		testutil.Ok(t, err)

		bComp, err := NewBucketCompactor(logger, sy, gc, comp, dir, bkt, 2)
		testutil.Ok(t, err)

		// Compaction on empty should not fail.
		testutil.Ok(t, bComp.Compact(ctx))
		testutil.Equals(t, 0.0, promtest.ToFloat64(gc.metrics.garbageCollectedBlocks))
		testutil.Equals(t, 4, promtest.CollectAndCount(markedForDeletion))
		testutil.Equals(t, 0.0, promtest.ToFloat64(markedForDeletion.WithLabelValues(string(metadata.BetweenCompactDuplicateReason))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(markedForDeletion.WithLabelValues(string(metadata.PostCompactDuplicateDeletion))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(markedForDeletion.WithLabelValues(string(metadata.RetentionDeletion))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(markedForDeletion.WithLabelValues(string(metadata.PartialForTooLongDeletion))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(gc.metrics.garbageCollectionFailures))
		testutil.Equals(t, 0, promtest.CollectAndCount(sy.metrics.compactions))
		testutil.Equals(t, 0, promtest.CollectAndCount(sy.metrics.compactionRunsStarted))
		testutil.Equals(t, 0, promtest.CollectAndCount(sy.metrics.compactionRunsCompleted))
		testutil.Equals(t, 0, promtest.CollectAndCount(sy.metrics.compactionFailures))

		_, err = os.Stat(dir)
		testutil.Assert(t, os.IsNotExist(err), "dir %s should be remove after compaction.", dir)

		// Test label name with slash, regression: https://github.com/thanos-io/thanos/issues/1661.
		extLabels := labels.Labels{{Name: "e1", Value: "1/weird"}}
		extLabels2 := labels.Labels{{Name: "e1", Value: "1"}}
		metas := createAndUpload(t, bkt, []blockgenSpec{
			{
				numSamples: 100, mint: 0, maxt: 1000, extLset: extLabels, res: 124,
				series: []labels.Labels{
					{{Name: "a", Value: "1"}},
					{{Name: "a", Value: "2"}, {Name: "b", Value: "2"}},
					{{Name: "a", Value: "3"}},
					{{Name: "a", Value: "4"}},
				},
			},
			{
				numSamples: 100, mint: 2000, maxt: 3000, extLset: extLabels, res: 124,
				series: []labels.Labels{
					{{Name: "a", Value: "3"}},
					{{Name: "a", Value: "4"}},
					{{Name: "a", Value: "5"}},
					{{Name: "a", Value: "6"}},
				},
			},
			// Mix order to make sure compact is able to deduct min time / max time.
			// Currently TSDB does not produces empty blocks (see: https://github.com/prometheus/tsdb/pull/374). However before v2.7.0 it was
			// so we still want to mimick this case as close as possible.
			{
				mint: 1000, maxt: 2000, extLset: extLabels, res: 124,
				// Empty block.
			},
			// Due to TSDB compaction delay (not compacting fresh block), we need one more block to be pushed to trigger compaction.
			{
				numSamples: 100, mint: 3000, maxt: 4000, extLset: extLabels, res: 124,
				series: []labels.Labels{
					{{Name: "a", Value: "7"}},
				},
			},
			// Extra block for "distraction" for different resolution and one for different labels.
			{
				numSamples: 100, mint: 5000, maxt: 6000, extLset: labels.Labels{{Name: "e1", Value: "2"}}, res: 124,
				series: []labels.Labels{
					{{Name: "a", Value: "7"}},
				},
			},
			// Extra block for "distraction" for different resolution and one for different labels.
			{
				numSamples: 100, mint: 4000, maxt: 5000, extLset: extLabels, res: 0,
				series: []labels.Labels{
					{{Name: "a", Value: "7"}},
				},
			},
			// Second group (extLabels2).
			{
				numSamples: 100, mint: 2000, maxt: 3000, extLset: extLabels2, res: 124,
				series: []labels.Labels{
					{{Name: "a", Value: "3"}},
					{{Name: "a", Value: "4"}},
					{{Name: "a", Value: "6"}},
				},
			},
			{
				numSamples: 100, mint: 0, maxt: 1000, extLset: extLabels2, res: 124,
				series: []labels.Labels{
					{{Name: "a", Value: "1"}},
					{{Name: "a", Value: "2"}, {Name: "b", Value: "2"}},
					{{Name: "a", Value: "3"}},
					{{Name: "a", Value: "4"}},
				},
			},
			// Due to TSDB compaction delay (not compacting fresh block), we need one more block to be pushed to trigger compaction.
			{
				numSamples: 100, mint: 3000, maxt: 4000, extLset: extLabels2, res: 124,
				series: []labels.Labels{
					{{Name: "a", Value: "7"}},
				},
			},
		})

		testutil.Ok(t, bComp.Compact(ctx))
		testutil.Equals(t, 5.0, promtest.ToFloat64(gc.metrics.garbageCollectedBlocks))
		testutil.Equals(t, 4, promtest.CollectAndCount(markedForDeletion))
		testutil.Equals(t, 0.0, promtest.ToFloat64(markedForDeletion.WithLabelValues(string(metadata.BetweenCompactDuplicateReason))))
		testutil.Equals(t, 5.0, promtest.ToFloat64(markedForDeletion.WithLabelValues(string(metadata.PostCompactDuplicateDeletion))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(markedForDeletion.WithLabelValues(string(metadata.RetentionDeletion))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(markedForDeletion.WithLabelValues(string(metadata.PartialForTooLongDeletion))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(gc.metrics.garbageCollectionFailures))
		testutil.Equals(t, 4, promtest.CollectAndCount(sy.metrics.compactions))
		testutil.Equals(t, 1.0, promtest.ToFloat64(sy.metrics.compactions.WithLabelValues(GroupKey(metas[0].Thanos))))
		testutil.Equals(t, 1.0, promtest.ToFloat64(sy.metrics.compactions.WithLabelValues(GroupKey(metas[7].Thanos))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(sy.metrics.compactions.WithLabelValues(GroupKey(metas[4].Thanos))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(sy.metrics.compactions.WithLabelValues(GroupKey(metas[5].Thanos))))
		testutil.Equals(t, 4, promtest.CollectAndCount(sy.metrics.compactionRunsStarted))
		testutil.Equals(t, 2.0, promtest.ToFloat64(sy.metrics.compactionRunsStarted.WithLabelValues(GroupKey(metas[0].Thanos))))
		testutil.Equals(t, 2.0, promtest.ToFloat64(sy.metrics.compactionRunsStarted.WithLabelValues(GroupKey(metas[7].Thanos))))
		// TODO(bwplotka): Looks like we do some unnecessary loops. Not a major problem but investigate.
		testutil.Equals(t, 2.0, promtest.ToFloat64(sy.metrics.compactionRunsStarted.WithLabelValues(GroupKey(metas[4].Thanos))))
		testutil.Equals(t, 2.0, promtest.ToFloat64(sy.metrics.compactionRunsStarted.WithLabelValues(GroupKey(metas[5].Thanos))))
		testutil.Equals(t, 4, promtest.CollectAndCount(sy.metrics.compactionRunsCompleted))
		testutil.Equals(t, 2.0, promtest.ToFloat64(sy.metrics.compactionRunsCompleted.WithLabelValues(GroupKey(metas[0].Thanos))))
		testutil.Equals(t, 2.0, promtest.ToFloat64(sy.metrics.compactionRunsCompleted.WithLabelValues(GroupKey(metas[7].Thanos))))
		// TODO(bwplotka): Looks like we do some unnecessary loops. Not a major problem but investigate.
		testutil.Equals(t, 2.0, promtest.ToFloat64(sy.metrics.compactionRunsCompleted.WithLabelValues(GroupKey(metas[4].Thanos))))
		testutil.Equals(t, 2.0, promtest.ToFloat64(sy.metrics.compactionRunsCompleted.WithLabelValues(GroupKey(metas[5].Thanos))))
		testutil.Equals(t, 4, promtest.CollectAndCount(sy.metrics.compactionFailures))
		testutil.Equals(t, 0.0, promtest.ToFloat64(sy.metrics.compactionFailures.WithLabelValues(GroupKey(metas[0].Thanos))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(sy.metrics.compactionFailures.WithLabelValues(GroupKey(metas[7].Thanos))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(sy.metrics.compactionFailures.WithLabelValues(GroupKey(metas[4].Thanos))))
		testutil.Equals(t, 0.0, promtest.ToFloat64(sy.metrics.compactionFailures.WithLabelValues(GroupKey(metas[5].Thanos))))

		_, err = os.Stat(dir)
		testutil.Assert(t, os.IsNotExist(err), "dir %s should be remove after compaction.", dir)

		// Check object storage. All blocks that were included in new compacted one should be removed. New compacted ones
		// are present and looks as expected.
		nonCompactedExpected := map[ulid.ULID]bool{
			metas[3].ULID: false,
			metas[4].ULID: false,
			metas[5].ULID: false,
			metas[8].ULID: false,
		}
		others := map[string]metadata.Meta{}
		testutil.Ok(t, bkt.Iter(ctx, "", func(n string) error {
			id, ok := block.IsBlockDir(n)
			if !ok {
				return nil
			}

			if _, ok := nonCompactedExpected[id]; ok {
				nonCompactedExpected[id] = true
				return nil
			}

			meta, err := block.DownloadMeta(ctx, logger, bkt, id)
			if err != nil {
				return err
			}

			others[GroupKey(meta.Thanos)] = meta
			return nil
		}))

		for id, found := range nonCompactedExpected {
			testutil.Assert(t, found, "not found expected block %s", id.String())
		}

		// We expect two compacted blocks only outside of what we expected in `nonCompactedExpected`.
		testutil.Equals(t, 2, len(others))
		{
			meta, ok := others[groupKey(124, extLabels)]
			testutil.Assert(t, ok, "meta not found")

			testutil.Equals(t, int64(0), meta.MinTime)
			testutil.Equals(t, int64(3000), meta.MaxTime)
			testutil.Equals(t, uint64(6), meta.Stats.NumSeries)
			testutil.Equals(t, uint64(2*4*100), meta.Stats.NumSamples) // Only 2 times 4*100 because one block was empty.
			testutil.Equals(t, 2, meta.Compaction.Level)
			testutil.Equals(t, []ulid.ULID{metas[0].ULID, metas[1].ULID, metas[2].ULID}, meta.Compaction.Sources)

			// Check thanos meta.
			testutil.Assert(t, labels.Equal(extLabels, labels.FromMap(meta.Thanos.Labels)), "ext labels does not match")
			testutil.Equals(t, int64(124), meta.Thanos.Downsample.Resolution)
		}
		{
			meta, ok := others[groupKey(124, extLabels2)]
			testutil.Assert(t, ok, "meta not found")

			testutil.Equals(t, int64(0), meta.MinTime)
			testutil.Equals(t, int64(3000), meta.MaxTime)
			testutil.Equals(t, uint64(5), meta.Stats.NumSeries)
			testutil.Equals(t, uint64(2*4*100-100), meta.Stats.NumSamples)
			testutil.Equals(t, 2, meta.Compaction.Level)
			testutil.Equals(t, []ulid.ULID{metas[6].ULID, metas[7].ULID}, meta.Compaction.Sources)

			// Check thanos meta.
			testutil.Assert(t, labels.Equal(extLabels2, labels.FromMap(meta.Thanos.Labels)), "ext labels does not match")
			testutil.Equals(t, int64(124), meta.Thanos.Downsample.Resolution)
		}
	})
}

type blockgenSpec struct {
	mint, maxt int64
	series     []labels.Labels
	numSamples int
	extLset    labels.Labels
	res        int64
}

func createAndUpload(t testing.TB, bkt objstore.Bucket, blocks []blockgenSpec) (metas []*metadata.Meta) {
	prepareDir, err := ioutil.TempDir("", "test-compact-prepare")
	testutil.Ok(t, err)
	defer func() { testutil.Ok(t, os.RemoveAll(prepareDir)) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, b := range blocks {
		var id ulid.ULID
		var err error
		if b.numSamples == 0 {
			id, err = e2eutil.CreateEmptyBlock(prepareDir, b.mint, b.maxt, b.extLset, b.res)
		} else {
			id, err = e2eutil.CreateBlock(ctx, prepareDir, b.series, b.numSamples, b.mint, b.maxt, b.extLset, b.res)
		}
		testutil.Ok(t, err)

		meta, err := metadata.Read(filepath.Join(prepareDir, id.String()))
		testutil.Ok(t, err)
		metas = append(metas, meta)

		testutil.Ok(t, block.Upload(ctx, log.NewNopLogger(), bkt, filepath.Join(prepareDir, id.String())))
	}
	return metas
}
