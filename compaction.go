// Copyright 2013 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"math"
	"runtime/pprof"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/compact"
	"github.com/cockroachdb/pebble/internal/invalidating"
	"github.com/cockroachdb/pebble/internal/invariants"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/internal/keyspan/keyspanimpl"
	"github.com/cockroachdb/pebble/internal/manifest"
	"github.com/cockroachdb/pebble/internal/private"
	"github.com/cockroachdb/pebble/internal/rangedel"
	"github.com/cockroachdb/pebble/internal/rangekey"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider/objiotracing"
	"github.com/cockroachdb/pebble/objstorage/remote"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/pebble/wal"
)

var errEmptyTable = errors.New("pebble: empty table")

// ErrCancelledCompaction is returned if a compaction is cancelled by a
// concurrent excise or ingest-split operation.
var ErrCancelledCompaction = errors.New("pebble: compaction cancelled by a concurrent operation, will retry compaction")

var compactLabels = pprof.Labels("pebble", "compact")
var flushLabels = pprof.Labels("pebble", "flush")
var gcLabels = pprof.Labels("pebble", "gc")

// getInternalWriterProperties accesses a private variable (in the
// internal/private package) initialized by the sstable Writer. This indirection
// is necessary to ensure non-Pebble users constructing sstables for ingestion
// are unable to set internal-only properties.
var getInternalWriterProperties = private.SSTableInternalProperties.(func(*sstable.Writer) *sstable.Properties)

// expandedCompactionByteSizeLimit is the maximum number of bytes in all
// compacted files. We avoid expanding the lower level file set of a compaction
// if it would make the total compaction cover more than this many bytes.
func expandedCompactionByteSizeLimit(opts *Options, level int, availBytes uint64) uint64 {
	v := uint64(25 * opts.Level(level).TargetFileSize)

	// Never expand a compaction beyond half the available capacity, divided
	// by the maximum number of concurrent compactions. Each of the concurrent
	// compactions may expand up to this limit, so this attempts to limit
	// compactions to half of available disk space. Note that this will not
	// prevent compaction picking from pursuing compactions that are larger
	// than this threshold before expansion.
	diskMax := (availBytes / 2) / uint64(opts.MaxConcurrentCompactions())
	if v > diskMax {
		v = diskMax
	}
	return v
}

// maxGrandparentOverlapBytes is the maximum bytes of overlap with level+1
// before we stop building a single file in a level-1 to level compaction.
func maxGrandparentOverlapBytes(opts *Options, level int) uint64 {
	return uint64(10 * opts.Level(level).TargetFileSize)
}

// maxReadCompactionBytes is used to prevent read compactions which
// are too wide.
func maxReadCompactionBytes(opts *Options, level int) uint64 {
	return uint64(10 * opts.Level(level).TargetFileSize)
}

// noCloseIter wraps around a FragmentIterator, intercepting and eliding
// calls to Close. It is used during compaction to ensure that rangeDelIters
// are not closed prematurely.
type noCloseIter struct {
	keyspan.FragmentIterator
}

func (i noCloseIter) Close() error {
	return nil
}

type compactionLevel struct {
	level int
	files manifest.LevelSlice
	// l0SublevelInfo contains information about L0 sublevels being compacted.
	// It's only set for the start level of a compaction starting out of L0 and
	// is nil for all other compactions.
	l0SublevelInfo []sublevelInfo
}

func (cl compactionLevel) Clone() compactionLevel {
	newCL := compactionLevel{
		level: cl.level,
		files: cl.files.Reslice(func(start, end *manifest.LevelIterator) {}),
	}
	return newCL
}
func (cl compactionLevel) String() string {
	return fmt.Sprintf(`Level %d, Files %s`, cl.level, cl.files)
}

// compactionWritable is a objstorage.Writable wrapper that, on every write,
// updates a metric in `versions` on bytes written by in-progress compactions so
// far. It also increments a per-compaction `written` int.
type compactionWritable struct {
	objstorage.Writable

	versions *versionSet
	written  *int64
}

// Write is part of the objstorage.Writable interface.
func (c *compactionWritable) Write(p []byte) error {
	if err := c.Writable.Write(p); err != nil {
		return err
	}

	*c.written += int64(len(p))
	c.versions.incrementCompactionBytes(int64(len(p)))
	return nil
}

type compactionKind int

const (
	compactionKindDefault compactionKind = iota
	compactionKindFlush
	// compactionKindMove denotes a move compaction where the input file is
	// retained and linked in a new level without being obsoleted.
	compactionKindMove
	// compactionKindCopy denotes a copy compaction where the input file is
	// copied byte-by-byte into a new file with a new FileNum in the output level.
	compactionKindCopy
	// compactionKindDeleteOnly denotes a compaction that only deletes input
	// files. It can occur when wide range tombstones completely contain sstables.
	compactionKindDeleteOnly
	compactionKindElisionOnly
	compactionKindRead
	compactionKindRewrite
	compactionKindIngestedFlushable
)

func (k compactionKind) String() string {
	switch k {
	case compactionKindDefault:
		return "default"
	case compactionKindFlush:
		return "flush"
	case compactionKindMove:
		return "move"
	case compactionKindDeleteOnly:
		return "delete-only"
	case compactionKindElisionOnly:
		return "elision-only"
	case compactionKindRead:
		return "read"
	case compactionKindRewrite:
		return "rewrite"
	case compactionKindIngestedFlushable:
		return "ingested-flushable"
	case compactionKindCopy:
		return "copy"
	}
	return "?"
}

// rangeKeyCompactionTransform is used to transform range key spans as part of the
// keyspanimpl.MergingIter. As part of this transformation step, we can elide range
// keys in the last snapshot stripe, as well as coalesce range keys within
// snapshot stripes.
func rangeKeyCompactionTransform(
	eq base.Equal, snapshots []uint64, elideRangeKey func(start, end []byte) bool,
) keyspan.Transformer {
	return keyspan.TransformerFunc(func(cmp base.Compare, s keyspan.Span, dst *keyspan.Span) error {
		elideInLastStripe := func(keys []keyspan.Key) []keyspan.Key {
			// Unsets and deletes in the last snapshot stripe can be elided.
			k := 0
			for j := range keys {
				if elideRangeKey(s.Start, s.End) &&
					(keys[j].Kind() == InternalKeyKindRangeKeyUnset || keys[j].Kind() == InternalKeyKindRangeKeyDelete) {
					continue
				}
				keys[k] = keys[j]
				k++
			}
			keys = keys[:k]
			return keys
		}
		// snapshots are in ascending order, while s.keys are in descending seqnum
		// order. Partition s.keys by snapshot stripes, and call rangekey.Coalesce
		// on each partition.
		dst.Start = s.Start
		dst.End = s.End
		dst.Keys = dst.Keys[:0]
		i, j := len(snapshots)-1, 0
		usedLen := 0
		for i >= 0 {
			start := j
			for j < len(s.Keys) && !base.Visible(s.Keys[j].SeqNum(), snapshots[i], base.InternalKeySeqNumMax) {
				// Include j in current partition.
				j++
			}
			if j > start {
				keysDst := dst.Keys[usedLen:cap(dst.Keys)]
				rangekey.Coalesce(cmp, eq, s.Keys[start:j], &keysDst)
				if j == len(s.Keys) {
					// This is the last snapshot stripe. Unsets and deletes can be elided.
					keysDst = elideInLastStripe(keysDst)
				}
				usedLen += len(keysDst)
				dst.Keys = append(dst.Keys, keysDst...)
			}
			i--
		}
		if j < len(s.Keys) {
			keysDst := dst.Keys[usedLen:cap(dst.Keys)]
			rangekey.Coalesce(cmp, eq, s.Keys[j:], &keysDst)
			keysDst = elideInLastStripe(keysDst)
			usedLen += len(keysDst)
			dst.Keys = append(dst.Keys, keysDst...)
		}
		return nil
	})
}

// compaction is a table compaction from one level to the next, starting from a
// given version.
type compaction struct {
	// cancel is a bool that can be used by other goroutines to signal a compaction
	// to cancel, such as if a conflicting excise operation raced it to manifest
	// application. Only holders of the manifest lock will write to this atomic.
	cancel atomic.Bool

	kind compactionKind
	// isDownload is true if this compaction was started as part of a Download
	// operation. In this case kind is compactionKindCopy or
	// compactionKindRewrite.
	isDownload bool

	cmp       Compare
	equal     Equal
	comparer  *base.Comparer
	formatKey base.FormatKey
	logger    Logger
	version   *version
	stats     base.InternalIteratorStats
	beganAt   time.Time
	// versionEditApplied is set to true when a compaction has completed and the
	// resulting version has been installed (if successful), but the compaction
	// goroutine is still cleaning up (eg, deleting obsolete files).
	versionEditApplied bool
	bufferPool         sstable.BufferPool

	// startLevel is the level that is being compacted. Inputs from startLevel
	// and outputLevel will be merged to produce a set of outputLevel files.
	startLevel *compactionLevel

	// outputLevel is the level that files are being produced in. outputLevel is
	// equal to startLevel+1 except when:
	//    - if startLevel is 0, the output level equals compactionPicker.baseLevel().
	//    - in multilevel compaction, the output level is the lowest level involved in
	//      the compaction
	// A compaction's outputLevel is nil for delete-only compactions.
	outputLevel *compactionLevel

	// extraLevels point to additional levels in between the input and output
	// levels that get compacted in multilevel compactions
	extraLevels []*compactionLevel

	inputs []compactionLevel

	// maxOutputFileSize is the maximum size of an individual table created
	// during compaction.
	maxOutputFileSize uint64
	// maxOverlapBytes is the maximum number of bytes of overlap allowed for a
	// single output table with the tables in the grandparent level.
	maxOverlapBytes uint64
	// disableSpanElision disables elision of range tombstones and range keys. Used
	// by tests to allow range tombstones or range keys to be added to tables where
	// they would otherwise be elided.
	disableSpanElision bool

	// flushing contains the flushables (aka memtables) that are being flushed.
	flushing flushableList
	// bytesWritten contains the number of bytes that have been written to outputs.
	bytesWritten int64

	// The boundaries of the input data.
	smallest InternalKey
	largest  InternalKey

	// rangeDelInterlaving is an interleaving iterator for range deletions, that
	// interleaves range tombstones among the point keys.
	rangeDelInterleaving keyspan.InterleavingIter
	// rangeKeyInterleaving is the interleaving iter for range keys.
	rangeKeyInterleaving keyspan.InterleavingIter

	// A list of objects to close when the compaction finishes. Used by input
	// iteration to keep rangeDelIters open for the lifetime of the compaction,
	// and only close them when the compaction finishes.
	closers []io.Closer

	// grandparents are the tables in level+2 that overlap with the files being
	// compacted. Used to determine output table boundaries. Do not assume that the actual files
	// in the grandparent when this compaction finishes will be the same.
	grandparents manifest.LevelSlice

	// Boundaries at which flushes to L0 should be split. Determined by
	// L0Sublevels. If nil, flushes aren't split.
	l0Limits [][]byte

	// List of disjoint inuse key ranges the compaction overlaps with in
	// grandparent and lower levels. See setupInuseKeyRanges() for the
	// construction. Used by elideTombstone() and elideRangeTombstone() to
	// determine if keys affected by a tombstone possibly exist at a lower level.
	inuseKeyRanges []manifest.UserKeyRange
	// inuseEntireRange is set if the above inuse key ranges wholly contain the
	// compaction's key range. This allows compactions in higher levels to often
	// elide key comparisons.
	inuseEntireRange    bool
	elideTombstoneIndex int

	// allowedZeroSeqNum is true if seqnums can be zeroed if there are no
	// snapshots requiring them to be kept. This determination is made by
	// looking for an sstable which overlaps the bounds of the compaction at a
	// lower level in the LSM during runCompaction.
	allowedZeroSeqNum bool

	metrics map[int]*LevelMetrics

	pickerMetrics compactionPickerMetrics
}

func (c *compaction) makeInfo(jobID JobID) CompactionInfo {
	info := CompactionInfo{
		JobID:       int(jobID),
		Reason:      c.kind.String(),
		Input:       make([]LevelInfo, 0, len(c.inputs)),
		Annotations: []string{},
	}
	if c.isDownload {
		info.Reason = "download," + info.Reason
	}
	for _, cl := range c.inputs {
		inputInfo := LevelInfo{Level: cl.level, Tables: nil}
		iter := cl.files.Iter()
		for m := iter.First(); m != nil; m = iter.Next() {
			inputInfo.Tables = append(inputInfo.Tables, m.TableInfo())
		}
		info.Input = append(info.Input, inputInfo)
	}
	if c.outputLevel != nil {
		info.Output.Level = c.outputLevel.level

		// If there are no inputs from the output level (eg, a move
		// compaction), add an empty LevelInfo to info.Input.
		if len(c.inputs) > 0 && c.inputs[len(c.inputs)-1].level != c.outputLevel.level {
			info.Input = append(info.Input, LevelInfo{Level: c.outputLevel.level})
		}
	} else {
		// For a delete-only compaction, set the output level to L6. The
		// output level is not meaningful here, but complicating the
		// info.Output interface with a pointer doesn't seem worth the
		// semantic distinction.
		info.Output.Level = numLevels - 1
	}

	for i, score := range c.pickerMetrics.scores {
		info.Input[i].Score = score
	}
	info.SingleLevelOverlappingRatio = c.pickerMetrics.singleLevelOverlappingRatio
	info.MultiLevelOverlappingRatio = c.pickerMetrics.multiLevelOverlappingRatio
	if len(info.Input) > 2 {
		info.Annotations = append(info.Annotations, "multilevel")
	}
	return info
}

func (c *compaction) userKeyBounds() base.UserKeyBounds {
	return base.UserKeyBoundsFromInternal(c.smallest, c.largest)
}

func newCompaction(
	pc *pickedCompaction, opts *Options, beganAt time.Time, provider objstorage.Provider,
) *compaction {
	c := &compaction{
		kind:              compactionKindDefault,
		cmp:               pc.cmp,
		equal:             opts.Comparer.Equal,
		comparer:          opts.Comparer,
		formatKey:         opts.Comparer.FormatKey,
		inputs:            pc.inputs,
		smallest:          pc.smallest,
		largest:           pc.largest,
		logger:            opts.Logger,
		version:           pc.version,
		beganAt:           beganAt,
		maxOutputFileSize: pc.maxOutputFileSize,
		maxOverlapBytes:   pc.maxOverlapBytes,
		pickerMetrics:     pc.pickerMetrics,
	}
	c.startLevel = &c.inputs[0]
	if pc.startLevel.l0SublevelInfo != nil {
		c.startLevel.l0SublevelInfo = pc.startLevel.l0SublevelInfo
	}
	c.outputLevel = &c.inputs[1]

	if len(pc.extraLevels) > 0 {
		c.extraLevels = pc.extraLevels
		c.outputLevel = &c.inputs[len(c.inputs)-1]
	}
	// Compute the set of outputLevel+1 files that overlap this compaction (these
	// are the grandparent sstables).
	if c.outputLevel.level+1 < numLevels {
		c.grandparents = c.version.Overlaps(c.outputLevel.level+1, c.userKeyBounds())
	}
	c.setupInuseKeyRanges()
	c.kind = pc.kind

	if c.kind == compactionKindDefault && c.outputLevel.files.Empty() && !c.hasExtraLevelData() &&
		c.startLevel.files.Len() == 1 && c.grandparents.SizeSum() <= c.maxOverlapBytes {
		// This compaction can be converted into a move or copy from one level
		// to the next. We avoid such a move if there is lots of overlapping
		// grandparent data. Otherwise, the move could create a parent file
		// that will require a very expensive merge later on.
		iter := c.startLevel.files.Iter()
		meta := iter.First()
		isRemote := false
		// We should always be passed a provider, except in some unit tests.
		if provider != nil {
			isRemote = !objstorage.IsLocalTable(provider, meta.FileBacking.DiskFileNum)
		}
		// Avoid a trivial move or copy if all of these are true, as rewriting a
		// new file is better:
		//
		// 1) The source file is a virtual sstable
		// 2) The existing file `meta` is on non-remote storage
		// 3) The output level prefers shared storage
		mustCopy := !isRemote && remote.ShouldCreateShared(opts.Experimental.CreateOnShared, c.outputLevel.level)
		if mustCopy {
			// If the source is virtual, it's best to just rewrite the file as all
			// conditions in the above comment are met.
			if !meta.Virtual {
				c.kind = compactionKindCopy
			}
		} else {
			c.kind = compactionKindMove
		}
	}
	return c
}

func newDeleteOnlyCompaction(
	opts *Options, cur *version, inputs []compactionLevel, beganAt time.Time,
) *compaction {
	c := &compaction{
		kind:      compactionKindDeleteOnly,
		cmp:       opts.Comparer.Compare,
		equal:     opts.Comparer.Equal,
		comparer:  opts.Comparer,
		formatKey: opts.Comparer.FormatKey,
		logger:    opts.Logger,
		version:   cur,
		beganAt:   beganAt,
		inputs:    inputs,
	}

	// Set c.smallest, c.largest.
	files := make([]manifest.LevelIterator, 0, len(inputs))
	for _, in := range inputs {
		files = append(files, in.files.Iter())
	}
	c.smallest, c.largest = manifest.KeyRange(opts.Comparer.Compare, files...)
	return c
}

func adjustGrandparentOverlapBytesForFlush(c *compaction, flushingBytes uint64) {
	// Heuristic to place a lower bound on compaction output file size
	// caused by Lbase. Prior to this heuristic we have observed an L0 in
	// production with 310K files of which 290K files were < 10KB in size.
	// Our hypothesis is that it was caused by L1 having 2600 files and
	// ~10GB, such that each flush got split into many tiny files due to
	// overlapping with most of the files in Lbase.
	//
	// The computation below is general in that it accounts
	// for flushing different volumes of data (e.g. we may be flushing
	// many memtables). For illustration, we consider the typical
	// example of flushing a 64MB memtable. So 12.8MB output,
	// based on the compression guess below. If the compressed bytes
	// guess is an over-estimate we will end up with smaller files,
	// and if an under-estimate we will end up with larger files.
	// With a 2MB target file size, 7 files. We are willing to accept
	// 4x the number of files, if it results in better write amplification
	// when later compacting to Lbase, i.e., ~450KB files (target file
	// size / 4).
	//
	// Note that this is a pessimistic heuristic in that
	// fileCountUpperBoundDueToGrandparents could be far from the actual
	// number of files produced due to the grandparent limits. For
	// example, in the extreme, consider a flush that overlaps with 1000
	// files in Lbase f0...f999, and the initially calculated value of
	// maxOverlapBytes will cause splits at f10, f20,..., f990, which
	// means an upper bound file count of 100 files. Say the input bytes
	// in the flush are such that acceptableFileCount=10. We will fatten
	// up maxOverlapBytes by 10x to ensure that the upper bound file count
	// drops to 10. However, it is possible that in practice, even without
	// this change, we would have produced no more than 10 files, and that
	// this change makes the files unnecessarily wide. Say the input bytes
	// are distributed such that 10% are in f0...f9, 10% in f10...f19, ...
	// 10% in f80...f89 and 10% in f990...f999. The original value of
	// maxOverlapBytes would have actually produced only 10 sstables. But
	// by increasing maxOverlapBytes by 10x, we may produce 1 sstable that
	// spans f0...f89, i.e., a much wider sstable than necessary.
	//
	// We could produce a tighter estimate of
	// fileCountUpperBoundDueToGrandparents if we had knowledge of the key
	// distribution of the flush. The 4x multiplier mentioned earlier is
	// a way to try to compensate for this pessimism.
	//
	// TODO(sumeer): we don't have compression info for the data being
	// flushed, but it is likely that existing files that overlap with
	// this flush in Lbase are representative wrt compression ratio. We
	// could store the uncompressed size in FileMetadata and estimate
	// the compression ratio.
	const approxCompressionRatio = 0.2
	approxOutputBytes := approxCompressionRatio * float64(flushingBytes)
	approxNumFilesBasedOnTargetSize :=
		int(math.Ceil(approxOutputBytes / float64(c.maxOutputFileSize)))
	acceptableFileCount := float64(4 * approxNumFilesBasedOnTargetSize)
	// The byte calculation is linear in numGrandparentFiles, but we will
	// incur this linear cost in findGrandparentLimit too, so we are also
	// willing to pay it now. We could approximate this cheaply by using
	// the mean file size of Lbase.
	grandparentFileBytes := c.grandparents.SizeSum()
	fileCountUpperBoundDueToGrandparents :=
		float64(grandparentFileBytes) / float64(c.maxOverlapBytes)
	if fileCountUpperBoundDueToGrandparents > acceptableFileCount {
		c.maxOverlapBytes = uint64(
			float64(c.maxOverlapBytes) *
				(fileCountUpperBoundDueToGrandparents / acceptableFileCount))
	}
}

func newFlush(
	opts *Options, cur *version, baseLevel int, flushing flushableList, beganAt time.Time,
) (*compaction, error) {
	c := &compaction{
		kind:              compactionKindFlush,
		cmp:               opts.Comparer.Compare,
		equal:             opts.Comparer.Equal,
		comparer:          opts.Comparer,
		formatKey:         opts.Comparer.FormatKey,
		logger:            opts.Logger,
		version:           cur,
		beganAt:           beganAt,
		inputs:            []compactionLevel{{level: -1}, {level: 0}},
		maxOutputFileSize: math.MaxUint64,
		maxOverlapBytes:   math.MaxUint64,
		flushing:          flushing,
	}
	c.startLevel = &c.inputs[0]
	c.outputLevel = &c.inputs[1]

	if len(flushing) > 0 {
		if _, ok := flushing[0].flushable.(*ingestedFlushable); ok {
			if len(flushing) != 1 {
				panic("pebble: ingestedFlushable must be flushed one at a time.")
			}
			c.kind = compactionKindIngestedFlushable
			return c, nil
		}
	}

	// Make sure there's no ingestedFlushable after the first flushable in the
	// list.
	for _, f := range flushing {
		if _, ok := f.flushable.(*ingestedFlushable); ok {
			panic("pebble: flushing shouldn't contain ingestedFlushable flushable")
		}
	}

	if cur.L0Sublevels != nil {
		c.l0Limits = cur.L0Sublevels.FlushSplitKeys()
	}

	smallestSet, largestSet := false, false
	updatePointBounds := func(iter internalIterator) {
		if kv := iter.First(); kv != nil {
			if !smallestSet ||
				base.InternalCompare(c.cmp, c.smallest, kv.K) > 0 {
				smallestSet = true
				c.smallest = kv.K.Clone()
			}
		}
		if kv := iter.Last(); kv != nil {
			if !largestSet ||
				base.InternalCompare(c.cmp, c.largest, kv.K) < 0 {
				largestSet = true
				c.largest = kv.K.Clone()
			}
		}
	}

	updateRangeBounds := func(iter keyspan.FragmentIterator) error {
		// File bounds require s != nil && !s.Empty(). We only need to check for
		// s != nil here, as the memtable's FragmentIterator would never surface
		// empty spans.
		if s, err := iter.First(); err != nil {
			return err
		} else if s != nil {
			if key := s.SmallestKey(); !smallestSet ||
				base.InternalCompare(c.cmp, c.smallest, key) > 0 {
				smallestSet = true
				c.smallest = key.Clone()
			}
		}
		if s, err := iter.Last(); err != nil {
			return err
		} else if s != nil {
			if key := s.LargestKey(); !largestSet ||
				base.InternalCompare(c.cmp, c.largest, key) < 0 {
				largestSet = true
				c.largest = key.Clone()
			}
		}
		return nil
	}

	var flushingBytes uint64
	for i := range flushing {
		f := flushing[i]
		updatePointBounds(f.newIter(nil))
		if rangeDelIter := f.newRangeDelIter(nil); rangeDelIter != nil {
			if err := updateRangeBounds(rangeDelIter); err != nil {
				return nil, err
			}
		}
		if rangeKeyIter := f.newRangeKeyIter(nil); rangeKeyIter != nil {
			if err := updateRangeBounds(rangeKeyIter); err != nil {
				return nil, err
			}
		}
		flushingBytes += f.inuseBytes()
	}

	if opts.FlushSplitBytes > 0 {
		c.maxOutputFileSize = uint64(opts.Level(0).TargetFileSize)
		c.maxOverlapBytes = maxGrandparentOverlapBytes(opts, 0)
		c.grandparents = c.version.Overlaps(baseLevel, c.userKeyBounds())
		adjustGrandparentOverlapBytesForFlush(c, flushingBytes)
	}

	c.setupInuseKeyRanges()
	return c, nil
}

func (c *compaction) hasExtraLevelData() bool {
	if len(c.extraLevels) == 0 {
		// not a multi level compaction
		return false
	} else if c.extraLevels[0].files.Empty() {
		// a multi level compaction without data in the intermediate input level;
		// e.g. for a multi level compaction with levels 4,5, and 6, this could
		// occur if there is no files to compact in 5, or in 5 and 6 (i.e. a move).
		return false
	}
	return true
}

func (c *compaction) setupInuseKeyRanges() {
	level := c.outputLevel.level + 1
	if c.outputLevel.level == 0 {
		level = 0
	}
	// calculateInuseKeyRanges will return a series of sorted spans. Overlapping
	// or abutting spans have already been merged.
	c.inuseKeyRanges = c.version.CalculateInuseKeyRanges(
		level, numLevels-1, c.smallest.UserKey, c.largest.UserKey,
	)
	// Check if there's a single in-use span that encompasses the entire key
	// range of the compaction. This is an optimization to avoid key comparisons
	// against inuseKeyRanges during the compaction when every key within the
	// compaction overlaps with an in-use span.
	if len(c.inuseKeyRanges) > 0 {
		c.inuseEntireRange = c.cmp(c.inuseKeyRanges[0].Start, c.smallest.UserKey) <= 0 &&
			c.cmp(c.inuseKeyRanges[0].End, c.largest.UserKey) >= 0
	}
}

// findGrandparentLimit takes the start user key for a table and returns the
// user key to which that table can extend without excessively overlapping
// the grandparent level. If no limit is needed considering the grandparent
// files, this function returns nil. This is done in order to prevent a table
// at level N from overlapping too much data at level N+1. We want to avoid
// such large overlaps because they translate into large compactions. The
// current heuristic stops output of a table if the addition of another key
// would cause the table to overlap more than 10x the target file size at
// level N. See maxGrandparentOverlapBytes.
func (c *compaction) findGrandparentLimit(start []byte) []byte {
	iter := c.grandparents.Iter()
	var overlappedBytes uint64
	var greater bool
	for f := iter.SeekGE(c.cmp, start); f != nil; f = iter.Next() {
		overlappedBytes += f.Size
		// To ensure forward progress we always return a larger user
		// key than where we started. See comments above clients of
		// this function for how this is used.
		greater = greater || c.cmp(f.Smallest.UserKey, start) > 0
		if !greater {
			continue
		}

		// We return the smallest bound of a sstable rather than the
		// largest because the smallest is always inclusive, and limits
		// are used exlusively when truncating range tombstones. If we
		// truncated an output to the largest key while there's a
		// pending tombstone, the next output file would also overlap
		// the same grandparent f.
		if overlappedBytes > c.maxOverlapBytes {
			return f.Smallest.UserKey
		}
	}
	return nil
}

// findL0Limit takes the start key for a table and returns the user key to which
// that table can be extended without hitting the next l0Limit. Having flushed
// sstables "bridging across" an l0Limit could lead to increased L0 -> LBase
// compaction sizes as well as elevated read amplification.
func (c *compaction) findL0Limit(start []byte) []byte {
	if c.startLevel.level > -1 || c.outputLevel.level != 0 || len(c.l0Limits) == 0 {
		return nil
	}
	index := sort.Search(len(c.l0Limits), func(i int) bool {
		return c.cmp(c.l0Limits[i], start) > 0
	})
	if index < len(c.l0Limits) {
		return c.l0Limits[index]
	}
	return nil
}

// errorOnUserKeyOverlap returns an error if the last two written sstables in
// this compaction have revisions of the same user key present in both sstables,
// when it shouldn't (eg. when splitting flushes).
func (c *compaction) errorOnUserKeyOverlap(ve *versionEdit) error {
	if n := len(ve.NewFiles); n > 1 {
		meta := ve.NewFiles[n-1].Meta
		prevMeta := ve.NewFiles[n-2].Meta
		if !prevMeta.Largest.IsExclusiveSentinel() &&
			c.cmp(prevMeta.Largest.UserKey, meta.Smallest.UserKey) >= 0 {
			return errors.Errorf("pebble: compaction split user key across two sstables: %s in %s and %s",
				prevMeta.Largest.Pretty(c.formatKey),
				prevMeta.FileNum,
				meta.FileNum)
		}
	}
	return nil
}

// allowZeroSeqNum returns true if seqnum's can be zeroed if there are no
// snapshots requiring them to be kept. It performs this determination by
// looking for an sstable which overlaps the bounds of the compaction at a
// lower level in the LSM.
func (c *compaction) allowZeroSeqNum() bool {
	return c.elideRangeTombstone(c.smallest.UserKey, c.largest.UserKey)
}

// elideTombstone returns true if it is ok to elide a tombstone for the
// specified key. A return value of true guarantees that there are no key/value
// pairs at c.level+2 or higher that possibly contain the specified user
// key. The keys in multiple invocations to elideTombstone must be supplied in
// order.
func (c *compaction) elideTombstone(key []byte) bool {
	if c.inuseEntireRange || len(c.flushing) != 0 {
		return false
	}

	for ; c.elideTombstoneIndex < len(c.inuseKeyRanges); c.elideTombstoneIndex++ {
		r := &c.inuseKeyRanges[c.elideTombstoneIndex]
		if c.cmp(key, r.End) <= 0 {
			if c.cmp(key, r.Start) >= 0 {
				return false
			}
			break
		}
	}
	return true
}

// elideRangeTombstone returns true if it is ok to elide the specified range
// tombstone. A return value of true guarantees that there are no key/value
// pairs at c.outputLevel.level+1 or higher that possibly overlap the specified
// tombstone.
func (c *compaction) elideRangeTombstone(start, end []byte) bool {
	// Disable range tombstone elision if the testing knob for that is enabled,
	// or if we are flushing memtables. The latter requirement is due to
	// inuseKeyRanges not accounting for key ranges in other memtables that are
	// being flushed in the same compaction. It's possible for a range tombstone
	// in one memtable to overlap keys in a preceding memtable in c.flushing.
	//
	// This function is also used in setting allowZeroSeqNum, so disabling
	// elision of range tombstones also disables zeroing of SeqNums.
	//
	// TODO(peter): we disable zeroing of seqnums during flushing to match
	// RocksDB behavior and to avoid generating overlapping sstables during
	// DB.replayWAL. When replaying WAL files at startup, we flush after each
	// WAL is replayed building up a single version edit that is
	// applied. Because we don't apply the version edit after each flush, this
	// code doesn't know that L0 contains files and zeroing of seqnums should
	// be disabled. That is fixable, but it seems safer to just match the
	// RocksDB behavior for now.
	if c.disableSpanElision || len(c.flushing) != 0 {
		return false
	}

	lower := sort.Search(len(c.inuseKeyRanges), func(i int) bool {
		return c.cmp(c.inuseKeyRanges[i].End, start) >= 0
	})
	upper := sort.Search(len(c.inuseKeyRanges), func(i int) bool {
		return c.cmp(c.inuseKeyRanges[i].Start, end) > 0
	})
	return lower >= upper
}

// elideRangeKey returns true if it is ok to elide the specified range key. A
// return value of true guarantees that there are no key/value pairs at
// c.outputLevel.level+1 or higher that possibly overlap the specified range key.
func (c *compaction) elideRangeKey(start, end []byte) bool {
	// TODO(bilal): Track inuseKeyRanges separately for the range keyspace as
	// opposed to the point keyspace. Once that is done, elideRangeTombstone
	// can just check in the point keyspace, and this function can check for
	// inuseKeyRanges in the range keyspace.
	return c.elideRangeTombstone(start, end)
}

// newInputIter returns an iterator over all the input tables in a compaction.
func (c *compaction) newInputIter(
	newIters tableNewIters, newRangeKeyIter keyspanimpl.TableNewSpanIter, snapshots []uint64,
) (_ internalIterator, retErr error) {
	// Validate the ordering of compaction input files for defense in depth.
	if len(c.flushing) == 0 {
		if c.startLevel.level >= 0 {
			err := manifest.CheckOrdering(c.cmp, c.formatKey,
				manifest.Level(c.startLevel.level), c.startLevel.files.Iter())
			if err != nil {
				return nil, err
			}
		}
		err := manifest.CheckOrdering(c.cmp, c.formatKey,
			manifest.Level(c.outputLevel.level), c.outputLevel.files.Iter())
		if err != nil {
			return nil, err
		}
		if c.startLevel.level == 0 {
			if c.startLevel.l0SublevelInfo == nil {
				panic("l0SublevelInfo not created for compaction out of L0")
			}
			for _, info := range c.startLevel.l0SublevelInfo {
				err := manifest.CheckOrdering(c.cmp, c.formatKey,
					info.sublevel, info.Iter())
				if err != nil {
					return nil, err
				}
			}
		}
		if len(c.extraLevels) > 0 {
			if len(c.extraLevels) > 1 {
				panic("n>2 multi level compaction not implemented yet")
			}
			interLevel := c.extraLevels[0]
			err := manifest.CheckOrdering(c.cmp, c.formatKey,
				manifest.Level(interLevel.level), interLevel.files.Iter())
			if err != nil {
				return nil, err
			}
		}
	}

	// There are three classes of keys that a compaction needs to process: point
	// keys, range deletion tombstones and range keys. Collect all iterators for
	// all these classes of keys from all the levels. We'll aggregate them
	// together farther below.
	//
	// numInputLevels is an approximation of the number of iterator levels. Due
	// to idiosyncrasies in iterator construction, we may (rarely) exceed this
	// initial capacity.
	numInputLevels := max(len(c.flushing), len(c.inputs))
	iters := make([]internalIterator, 0, numInputLevels)
	rangeDelIters := make([]keyspan.FragmentIterator, 0, numInputLevels)
	rangeKeyIters := make([]keyspan.FragmentIterator, 0, numInputLevels)

	// If construction of the iterator inputs fails, ensure that we close all
	// the consitutent iterators.
	defer func() {
		if retErr != nil {
			for _, iter := range iters {
				if iter != nil {
					iter.Close()
				}
			}
			for _, rangeDelIter := range rangeDelIters {
				rangeDelIter.Close()
			}
		}
	}()
	iterOpts := IterOptions{
		CategoryAndQoS: sstable.CategoryAndQoS{
			Category: "pebble-compaction",
			QoSLevel: sstable.NonLatencySensitiveQoSLevel,
		},
		logger: c.logger,
	}

	// Populate iters, rangeDelIters and rangeKeyIters with the appropriate
	// constituent iterators. This depends on whether this is a flush or a
	// compaction.
	if len(c.flushing) != 0 {
		// If flushing, we need to build the input iterators over the memtables
		// stored in c.flushing.
		for i := range c.flushing {
			f := c.flushing[i]
			iters = append(iters, f.newFlushIter(nil))
			rangeDelIter := f.newRangeDelIter(nil)
			if rangeDelIter != nil {
				rangeDelIters = append(rangeDelIters, rangeDelIter)
			}
			if rangeKeyIter := f.newRangeKeyIter(nil); rangeKeyIter != nil {
				rangeKeyIters = append(rangeKeyIters, rangeKeyIter)
			}
		}
	} else {
		addItersForLevel := func(level *compactionLevel, l manifest.Level) error {
			// Add a *levelIter for point iterators. Because we don't call
			// initRangeDel, the levelIter will close and forget the range
			// deletion iterator when it steps on to a new file. Surfacing range
			// deletions to compactions are handled below.
			iters = append(iters, newLevelIter(context.Background(),
				iterOpts, c.comparer, newIters, level.files.Iter(), l, internalIterOpts{
					compaction: true,
					bufferPool: &c.bufferPool,
				}))
			// TODO(jackson): Use keyspanimpl.LevelIter to avoid loading all the range
			// deletions into memory upfront. (See #2015, which reverted this.) There
			// will be no user keys that are split between sstables within a level in
			// Cockroach 23.1, which unblocks this optimization.

			// Add the range deletion iterator for each file as an independent level
			// in mergingIter, as opposed to making a levelIter out of those. This
			// is safer as levelIter expects all keys coming from underlying
			// iterators to be in order. Due to compaction / tombstone writing
			// logic in finishOutput(), it is possible for range tombstones to not
			// be strictly ordered across all files in one level.
			//
			// Consider this example from the metamorphic tests (also repeated in
			// finishOutput()), consisting of three L3 files with their bounds
			// specified in square brackets next to the file name:
			//
			// ./000240.sst   [tmgc#391,MERGE-tmgc#391,MERGE]
			// tmgc#391,MERGE [786e627a]
			// tmgc-udkatvs#331,RANGEDEL
			//
			// ./000241.sst   [tmgc#384,MERGE-tmgc#384,MERGE]
			// tmgc#384,MERGE [666c7070]
			// tmgc-tvsalezade#383,RANGEDEL
			// tmgc-tvsalezade#331,RANGEDEL
			//
			// ./000242.sst   [tmgc#383,RANGEDEL-tvsalezade#72057594037927935,RANGEDEL]
			// tmgc-tvsalezade#383,RANGEDEL
			// tmgc#375,SET [72646c78766965616c72776865676e79]
			// tmgc-tvsalezade#356,RANGEDEL
			//
			// Here, the range tombstone in 000240.sst falls "after" one in
			// 000241.sst, despite 000240.sst being ordered "before" 000241.sst for
			// levelIter's purposes. While each file is still consistent before its
			// bounds, it's safer to have all rangedel iterators be visible to
			// mergingIter.
			iter := level.files.Iter()
			for f := iter.First(); f != nil; f = iter.Next() {
				rangeDelIter, closer, err := c.newRangeDelIter(
					newIters, iter.Take(), iterOpts, l)
				if err != nil {
					// The error will already be annotated with the BackingFileNum, so
					// we annotate it with the FileNum.
					return errors.Wrapf(err, "pebble: could not open table %s", errors.Safe(f.FileNum))
				}
				if rangeDelIter == nil {
					continue
				}
				rangeDelIters = append(rangeDelIters, rangeDelIter)
				c.closers = append(c.closers, closer)
			}

			// Check if this level has any range keys.
			hasRangeKeys := false
			for f := iter.First(); f != nil; f = iter.Next() {
				if f.HasRangeKeys {
					hasRangeKeys = true
					break
				}
			}
			if hasRangeKeys {
				li := &keyspanimpl.LevelIter{}
				newRangeKeyIterWrapper := func(file *manifest.FileMetadata, iterOptions keyspan.SpanIterOptions) (keyspan.FragmentIterator, error) {
					iter, err := newRangeKeyIter(file, iterOptions)
					if err != nil {
						return nil, err
					} else if iter == nil {
						return emptyKeyspanIter, nil
					}
					// Ensure that the range key iter is not closed until the compaction is
					// finished. This is necessary because range key processing
					// requires the range keys to be held in memory for up to the
					// lifetime of the compaction.
					c.closers = append(c.closers, iter)
					iter = noCloseIter{iter}

					// We do not need to truncate range keys to sstable boundaries, or
					// only read within the file's atomic compaction units, unlike with
					// range tombstones. This is because range keys were added after we
					// stopped splitting user keys across sstables, so all the range keys
					// in this sstable must wholly lie within the file's bounds.
					return iter, err
				}
				li.Init(keyspan.SpanIterOptions{}, c.cmp, newRangeKeyIterWrapper, level.files.Iter(), l, manifest.KeyTypeRange)
				rangeKeyIters = append(rangeKeyIters, li)
			}
			return nil
		}

		for i := range c.inputs {
			// If the level is annotated with l0SublevelInfo, expand it into one
			// level per sublevel.
			// TODO(jackson): Perform this expansion even earlier when we pick the
			// compaction?
			if len(c.inputs[i].l0SublevelInfo) > 0 {
				for _, info := range c.startLevel.l0SublevelInfo {
					sublevelCompactionLevel := &compactionLevel{0, info.LevelSlice, nil}
					if err := addItersForLevel(sublevelCompactionLevel, info.sublevel); err != nil {
						return nil, err
					}
				}
				continue
			}
			if err := addItersForLevel(&c.inputs[i], manifest.Level(c.inputs[i].level)); err != nil {
				return nil, err
			}
		}
	}

	// If there's only one constituent point iterator, we can avoid the overhead
	// of a *mergingIter. This is possible, for example, when performing a flush
	// of a single memtable. Otherwise, combine all the iterators into a merging
	// iter.
	iter := iters[0]
	if len(iters) > 1 {
		iter = newMergingIter(c.logger, &c.stats, c.cmp, nil, iters...)
	}

	// In normal operation, levelIter iterates over the point operations in a
	// level, and initializes a rangeDelIter pointer for the range deletions in
	// each table. During compaction, we want to iterate over the merged view of
	// point operations and range deletions. In order to do this we create one
	// levelIter per level to iterate over the point operations, and collect up
	// all the range deletion files.
	//
	// The range deletion levels are combined with a keyspanimpl.MergingIter. The
	// resulting merged rangedel iterator is then included using an
	// InterleavingIter.
	// TODO(jackson): Consider using a defragmenting iterator to stitch together
	// logical range deletions that were fragmented due to previous file
	// boundaries.
	if len(rangeDelIters) > 0 {
		mi := &keyspanimpl.MergingIter{}
		mi.Init(c.cmp, keyspan.NoopTransform, new(keyspanimpl.MergingBuffers), rangeDelIters...)
		c.rangeDelInterleaving.Init(c.comparer, iter, mi, keyspan.InterleavingIterOpts{})
		iter = &c.rangeDelInterleaving
	}

	// If there are range key iterators, we need to combine them using
	// keyspanimpl.MergingIter, and then interleave them among the points.
	if len(rangeKeyIters) > 0 {
		mi := &keyspanimpl.MergingIter{}
		mi.Init(c.cmp, rangeKeyCompactionTransform(c.equal, snapshots, c.elideRangeKey), new(keyspanimpl.MergingBuffers), rangeKeyIters...)
		di := &keyspan.DefragmentingIter{}
		di.Init(c.comparer, mi, keyspan.DefragmentInternal, keyspan.StaticDefragmentReducer, new(keyspan.DefragmentingBuffers))
		c.rangeKeyInterleaving.Init(c.comparer, iter, di, keyspan.InterleavingIterOpts{})
		iter = &c.rangeKeyInterleaving
	}
	return iter, nil
}

func (c *compaction) newRangeDelIter(
	newIters tableNewIters, f manifest.LevelFile, opts IterOptions, l manifest.Level,
) (keyspan.FragmentIterator, io.Closer, error) {
	opts.level = l
	iterSet, err := newIters(context.Background(), f.FileMetadata, &opts,
		internalIterOpts{
			compaction: true,
			bufferPool: &c.bufferPool,
		}, iterRangeDeletions)
	if err != nil {
		return nil, nil, err
	} else if iterSet.rangeDeletion == nil {
		// The file doesn't contain any range deletions.
		return nil, nil, nil
	}
	// Ensure that rangeDelIter is not closed until the compaction is
	// finished. This is necessary because range tombstone processing
	// requires the range tombstones to be held in memory for up to the
	// lifetime of the compaction.
	return noCloseIter{iterSet.rangeDeletion}, iterSet.rangeDeletion, nil
}

func (c *compaction) String() string {
	if len(c.flushing) != 0 {
		return "flush\n"
	}

	var buf bytes.Buffer
	for level := c.startLevel.level; level <= c.outputLevel.level; level++ {
		i := level - c.startLevel.level
		fmt.Fprintf(&buf, "%d:", level)
		iter := c.inputs[i].files.Iter()
		for f := iter.First(); f != nil; f = iter.Next() {
			fmt.Fprintf(&buf, " %s:%s-%s", f.FileNum, f.Smallest, f.Largest)
		}
		fmt.Fprintf(&buf, "\n")
	}
	return buf.String()
}

type manualCompaction struct {
	// Count of the retries either due to too many concurrent compactions, or a
	// concurrent compaction to overlapping levels.
	retries     int
	level       int
	outputLevel int
	done        chan error
	start       []byte
	end         []byte
	split       bool
}

type readCompaction struct {
	level int
	// [start, end] key ranges are used for de-duping.
	start []byte
	end   []byte

	// The file associated with the compaction.
	// If the file no longer belongs in the same
	// level, then we skip the compaction.
	fileNum base.FileNum
}

func (d *DB) addInProgressCompaction(c *compaction) {
	d.mu.compact.inProgress[c] = struct{}{}
	var isBase, isIntraL0 bool
	for _, cl := range c.inputs {
		iter := cl.files.Iter()
		for f := iter.First(); f != nil; f = iter.Next() {
			if f.IsCompacting() {
				d.opts.Logger.Fatalf("L%d->L%d: %s already being compacted", c.startLevel.level, c.outputLevel.level, f.FileNum)
			}
			f.SetCompactionState(manifest.CompactionStateCompacting)
			if c.startLevel != nil && c.outputLevel != nil && c.startLevel.level == 0 {
				if c.outputLevel.level == 0 {
					f.IsIntraL0Compacting = true
					isIntraL0 = true
				} else {
					isBase = true
				}
			}
		}
	}

	if (isIntraL0 || isBase) && c.version.L0Sublevels != nil {
		l0Inputs := []manifest.LevelSlice{c.startLevel.files}
		if isIntraL0 {
			l0Inputs = append(l0Inputs, c.outputLevel.files)
		}
		if err := c.version.L0Sublevels.UpdateStateForStartedCompaction(l0Inputs, isBase); err != nil {
			d.opts.Logger.Fatalf("could not update state for compaction: %s", err)
		}
	}
}

// Removes compaction markers from files in a compaction. The rollback parameter
// indicates whether the compaction state should be rolled back to its original
// state in the case of an unsuccessful compaction.
//
// DB.mu must be held when calling this method, however this method can drop and
// re-acquire that mutex. All writes to the manifest for this compaction should
// have completed by this point.
func (d *DB) clearCompactingState(c *compaction, rollback bool) {
	c.versionEditApplied = true
	for _, cl := range c.inputs {
		iter := cl.files.Iter()
		for f := iter.First(); f != nil; f = iter.Next() {
			if !f.IsCompacting() {
				d.opts.Logger.Fatalf("L%d->L%d: %s not being compacted", c.startLevel.level, c.outputLevel.level, f.FileNum)
			}
			if !rollback {
				// On success all compactions other than move-compactions transition the
				// file into the Compacted state. Move-compacted files become eligible
				// for compaction again and transition back to NotCompacting.
				if c.kind != compactionKindMove {
					f.SetCompactionState(manifest.CompactionStateCompacted)
				} else {
					f.SetCompactionState(manifest.CompactionStateNotCompacting)
				}
			} else {
				// Else, on rollback, all input files unconditionally transition back to
				// NotCompacting.
				f.SetCompactionState(manifest.CompactionStateNotCompacting)
			}
			f.IsIntraL0Compacting = false
		}
	}
	l0InProgress := inProgressL0Compactions(d.getInProgressCompactionInfoLocked(c))
	func() {
		// InitCompactingFileInfo requires that no other manifest writes be
		// happening in parallel with it, i.e. we're not in the midst of installing
		// another version. Otherwise, it's possible that we've created another
		// L0Sublevels instance, but not added it to the versions list, causing
		// all the indices in FileMetadata to be inaccurate. To ensure this,
		// grab the manifest lock.
		d.mu.versions.logLock()
		defer d.mu.versions.logUnlock()
		d.mu.versions.currentVersion().L0Sublevels.InitCompactingFileInfo(l0InProgress)
	}()
}

func (d *DB) calculateDiskAvailableBytes() uint64 {
	if space, err := d.opts.FS.GetDiskUsage(d.dirname); err == nil {
		d.diskAvailBytes.Store(space.AvailBytes)
		return space.AvailBytes
	} else if !errors.Is(err, vfs.ErrUnsupported) {
		d.opts.EventListener.BackgroundError(err)
	}
	return d.diskAvailBytes.Load()
}

func (d *DB) getDeletionPacerInfo() deletionPacerInfo {
	var pacerInfo deletionPacerInfo
	// Call GetDiskUsage after every file deletion. This may seem inefficient,
	// but in practice this was observed to take constant time, regardless of
	// volume size used, at least on linux with ext4 and zfs. All invocations
	// take 10 microseconds or less.
	pacerInfo.freeBytes = d.calculateDiskAvailableBytes()
	d.mu.Lock()
	pacerInfo.obsoleteBytes = d.mu.versions.metrics.Table.ObsoleteSize
	pacerInfo.liveBytes = uint64(d.mu.versions.metrics.Total().Size)
	d.mu.Unlock()
	return pacerInfo
}

// onObsoleteTableDelete is called to update metrics when an sstable is deleted.
func (d *DB) onObsoleteTableDelete(fileSize uint64, isLocal bool) {
	d.mu.Lock()
	d.mu.versions.metrics.Table.ObsoleteCount--
	d.mu.versions.metrics.Table.ObsoleteSize -= fileSize
	if isLocal {
		d.mu.versions.metrics.Table.Local.ObsoleteSize -= fileSize
	}
	d.mu.Unlock()
}

// maybeScheduleFlush schedules a flush if necessary.
//
// d.mu must be held when calling this.
func (d *DB) maybeScheduleFlush() {
	if d.mu.compact.flushing || d.closed.Load() != nil || d.opts.ReadOnly {
		return
	}
	if len(d.mu.mem.queue) <= 1 {
		return
	}

	if !d.passedFlushThreshold() {
		return
	}

	d.mu.compact.flushing = true
	go d.flush()
}

func (d *DB) passedFlushThreshold() bool {
	var n int
	var size uint64
	for ; n < len(d.mu.mem.queue)-1; n++ {
		if !d.mu.mem.queue[n].readyForFlush() {
			break
		}
		if d.mu.mem.queue[n].flushForced {
			// A flush was forced. Pretend the memtable size is the configured
			// size. See minFlushSize below.
			size += d.opts.MemTableSize
		} else {
			size += d.mu.mem.queue[n].totalBytes()
		}
	}
	if n == 0 {
		// None of the immutable memtables are ready for flushing.
		return false
	}

	// Only flush once the sum of the queued memtable sizes exceeds half the
	// configured memtable size. This prevents flushing of memtables at startup
	// while we're undergoing the ramp period on the memtable size. See
	// DB.newMemTable().
	minFlushSize := d.opts.MemTableSize / 2
	return size >= minFlushSize
}

func (d *DB) maybeScheduleDelayedFlush(tbl *memTable, dur time.Duration) {
	var mem *flushableEntry
	for _, m := range d.mu.mem.queue {
		if m.flushable == tbl {
			mem = m
			break
		}
	}
	if mem == nil || mem.flushForced {
		return
	}
	deadline := d.timeNow().Add(dur)
	if !mem.delayedFlushForcedAt.IsZero() && deadline.After(mem.delayedFlushForcedAt) {
		// Already scheduled to flush sooner than within `dur`.
		return
	}
	mem.delayedFlushForcedAt = deadline
	go func() {
		timer := time.NewTimer(dur)
		defer timer.Stop()

		select {
		case <-d.closedCh:
			return
		case <-mem.flushed:
			return
		case <-timer.C:
			d.commit.mu.Lock()
			defer d.commit.mu.Unlock()
			d.mu.Lock()
			defer d.mu.Unlock()

			// NB: The timer may fire concurrently with a call to Close.  If a
			// Close call beat us to acquiring d.mu, d.closed holds ErrClosed,
			// and it's too late to flush anything. Otherwise, the Close call
			// will block on locking d.mu until we've finished scheduling the
			// flush and set `d.mu.compact.flushing` to true. Close will wait
			// for the current flush to complete.
			if d.closed.Load() != nil {
				return
			}

			if d.mu.mem.mutable == tbl {
				d.makeRoomForWrite(nil)
			} else {
				mem.flushForced = true
			}
			d.maybeScheduleFlush()
		}
	}()
}

func (d *DB) flush() {
	pprof.Do(context.Background(), flushLabels, func(context.Context) {
		flushingWorkStart := time.Now()
		d.mu.Lock()
		defer d.mu.Unlock()
		idleDuration := flushingWorkStart.Sub(d.mu.compact.noOngoingFlushStartTime)
		var bytesFlushed uint64
		var err error
		if bytesFlushed, err = d.flush1(); err != nil {
			// TODO(peter): count consecutive flush errors and backoff.
			d.opts.EventListener.BackgroundError(err)
		}
		d.mu.compact.flushing = false
		d.mu.compact.noOngoingFlushStartTime = time.Now()
		workDuration := d.mu.compact.noOngoingFlushStartTime.Sub(flushingWorkStart)
		d.mu.compact.flushWriteThroughput.Bytes += int64(bytesFlushed)
		d.mu.compact.flushWriteThroughput.WorkDuration += workDuration
		d.mu.compact.flushWriteThroughput.IdleDuration += idleDuration
		// More flush work may have arrived while we were flushing, so schedule
		// another flush if needed.
		d.maybeScheduleFlush()
		// The flush may have produced too many files in a level, so schedule a
		// compaction if needed.
		d.maybeScheduleCompaction()
		d.mu.compact.cond.Broadcast()
	})
}

// runIngestFlush is used to generate a flush version edit for sstables which
// were ingested as flushables. Both DB.mu and the manifest lock must be held
// while runIngestFlush is called.
func (d *DB) runIngestFlush(c *compaction) (*manifest.VersionEdit, error) {
	if len(c.flushing) != 1 {
		panic("pebble: ingestedFlushable must be flushed one at a time.")
	}

	// Construct the VersionEdit, levelMetrics etc.
	c.metrics = make(map[int]*LevelMetrics, numLevels)
	// Finding the target level for ingestion must use the latest version
	// after the logLock has been acquired.
	c.version = d.mu.versions.currentVersion()

	baseLevel := d.mu.versions.picker.getBaseLevel()
	iterOpts := IterOptions{logger: d.opts.Logger}
	ve := &versionEdit{}
	var ingestSplitFiles []ingestSplitFile
	ingestFlushable := c.flushing[0].flushable.(*ingestedFlushable)

	updateLevelMetricsOnExcise := func(m *fileMetadata, level int, added []newFileEntry) {
		levelMetrics := c.metrics[level]
		if levelMetrics == nil {
			levelMetrics = &LevelMetrics{}
			c.metrics[level] = levelMetrics
		}
		levelMetrics.NumFiles--
		levelMetrics.Size -= int64(m.Size)
		for i := range added {
			levelMetrics.NumFiles++
			levelMetrics.Size += int64(added[i].Meta.Size)
		}
	}

	suggestSplit := d.opts.Experimental.IngestSplit != nil && d.opts.Experimental.IngestSplit() &&
		d.FormatMajorVersion() >= FormatVirtualSSTables

	if suggestSplit || ingestFlushable.exciseSpan.Valid() {
		// We could add deleted files to ve.
		ve.DeletedFiles = make(map[manifest.DeletedFileEntry]*manifest.FileMetadata)
	}

	replacedFiles := make(map[base.FileNum][]newFileEntry)
	for _, file := range ingestFlushable.files {
		var fileToSplit *fileMetadata
		var level int

		// This file fits perfectly within the excise span, so we can slot it at L6.
		if ingestFlushable.exciseSpan.Valid() &&
			ingestFlushable.exciseSpan.Contains(d.cmp, file.FileMetadata.Smallest) &&
			ingestFlushable.exciseSpan.Contains(d.cmp, file.FileMetadata.Largest) {
			level = 6
		} else {
			var err error
			level, fileToSplit, err = ingestTargetLevel(
				d.newIters, d.tableNewRangeKeyIter, iterOpts, d.opts.Comparer,
				c.version, baseLevel, d.mu.compact.inProgress, file.FileMetadata,
				suggestSplit,
			)
			if err != nil {
				return nil, err
			}
		}

		// Add the current flushableIngest file to the version.
		ve.NewFiles = append(ve.NewFiles, newFileEntry{Level: level, Meta: file.FileMetadata})
		if fileToSplit != nil {
			ingestSplitFiles = append(ingestSplitFiles, ingestSplitFile{
				ingestFile: file.FileMetadata,
				splitFile:  fileToSplit,
				level:      level,
			})
		}
		levelMetrics := c.metrics[level]
		if levelMetrics == nil {
			levelMetrics = &LevelMetrics{}
			c.metrics[level] = levelMetrics
		}
		levelMetrics.BytesIngested += file.Size
		levelMetrics.TablesIngested++
	}
	if ingestFlushable.exciseSpan.Valid() {
		// Iterate through all levels and find files that intersect with exciseSpan.
		for l := range c.version.Levels {
			overlaps := c.version.Overlaps(l, base.UserKeyBoundsEndExclusive(ingestFlushable.exciseSpan.Start, ingestFlushable.exciseSpan.End))
			iter := overlaps.Iter()

			for m := iter.First(); m != nil; m = iter.Next() {
				newFiles, err := d.excise(ingestFlushable.exciseSpan.UserKeyBounds(), m, ve, l)
				if err != nil {
					return nil, err
				}

				if _, ok := ve.DeletedFiles[deletedFileEntry{
					Level:   l,
					FileNum: m.FileNum,
				}]; !ok {
					// We did not excise this file.
					continue
				}
				replacedFiles[m.FileNum] = newFiles
				updateLevelMetricsOnExcise(m, l, newFiles)
			}
		}
	}

	if len(ingestSplitFiles) > 0 {
		if err := d.ingestSplit(ve, updateLevelMetricsOnExcise, ingestSplitFiles, replacedFiles); err != nil {
			return nil, err
		}
	}

	return ve, nil
}

// flush runs a compaction that copies the immutable memtables from memory to
// disk.
//
// d.mu must be held when calling this, but the mutex may be dropped and
// re-acquired during the course of this method.
func (d *DB) flush1() (bytesFlushed uint64, err error) {
	// NB: The flushable queue can contain flushables of type ingestedFlushable.
	// The sstables in ingestedFlushable.files must be placed into the appropriate
	// level in the lsm. Let's say the flushable queue contains a prefix of
	// regular immutable memtables, then an ingestedFlushable, and then the
	// mutable memtable. When the flush of the ingestedFlushable is performed,
	// it needs an updated view of the lsm. That is, the prefix of immutable
	// memtables must have already been flushed. Similarly, if there are two
	// contiguous ingestedFlushables in the queue, then the first flushable must
	// be flushed, so that the second flushable can see an updated view of the
	// lsm.
	//
	// Given the above, we restrict flushes to either some prefix of regular
	// memtables, or a single flushable of type ingestedFlushable. The DB.flush
	// function will call DB.maybeScheduleFlush again, so a new flush to finish
	// the remaining flush work should be scheduled right away.
	//
	// NB: Large batches placed in the flushable queue share the WAL with the
	// previous memtable in the queue. We must ensure the property that both the
	// large batch and the memtable with which it shares a WAL are flushed
	// together. The property ensures that the minimum unflushed log number
	// isn't incremented incorrectly. Since a flushableBatch.readyToFlush always
	// returns true, and since the large batch will always be placed right after
	// the memtable with which it shares a WAL, the property is naturally
	// ensured. The large batch will always be placed after the memtable with
	// which it shares a WAL because we ensure it in DB.commitWrite by holding
	// the commitPipeline.mu and then holding DB.mu. As an extra defensive
	// measure, if we try to flush the memtable without also flushing the
	// flushable batch in the same flush, since the memtable and flushableBatch
	// have the same logNum, the logNum invariant check below will trigger.
	var n, inputs int
	var inputBytes uint64
	var ingest bool
	for ; n < len(d.mu.mem.queue)-1; n++ {
		if f, ok := d.mu.mem.queue[n].flushable.(*ingestedFlushable); ok {
			if n == 0 {
				// The first flushable is of type ingestedFlushable. Since these
				// must be flushed individually, we perform a flush for just
				// this.
				if !f.readyForFlush() {
					// This check is almost unnecessary, but we guard against it
					// just in case this invariant changes in the future.
					panic("pebble: ingestedFlushable should always be ready to flush.")
				}
				// By setting n = 1, we ensure that the first flushable(n == 0)
				// is scheduled for a flush. The number of tables added is equal to the
				// number of files in the ingest operation.
				n = 1
				inputs = len(f.files)
				ingest = true
				break
			} else {
				// There was some prefix of flushables which weren't of type
				// ingestedFlushable. So, perform a flush for those.
				break
			}
		}
		if !d.mu.mem.queue[n].readyForFlush() {
			break
		}
		inputBytes += d.mu.mem.queue[n].inuseBytes()
	}
	if n == 0 {
		// None of the immutable memtables are ready for flushing.
		return 0, nil
	}
	if !ingest {
		// Flushes of memtables add the prefix of n memtables from the flushable
		// queue.
		inputs = n
	}

	// Require that every memtable being flushed has a log number less than the
	// new minimum unflushed log number.
	minUnflushedLogNum := d.mu.mem.queue[n].logNum
	if !d.opts.DisableWAL {
		for i := 0; i < n; i++ {
			if logNum := d.mu.mem.queue[i].logNum; logNum >= minUnflushedLogNum {
				panic(errors.AssertionFailedf("logNum invariant violated: flushing %d items; %d:type=%T,logNum=%d; %d:type=%T,logNum=%d",
					n,
					i, d.mu.mem.queue[i].flushable, logNum,
					n, d.mu.mem.queue[n].flushable, minUnflushedLogNum))
			}
		}
	}

	c, err := newFlush(d.opts, d.mu.versions.currentVersion(),
		d.mu.versions.picker.getBaseLevel(), d.mu.mem.queue[:n], d.timeNow())
	if err != nil {
		return 0, err
	}
	d.addInProgressCompaction(c)

	jobID := d.newJobIDLocked()
	d.opts.EventListener.FlushBegin(FlushInfo{
		JobID:      int(jobID),
		Input:      inputs,
		InputBytes: inputBytes,
		Ingest:     ingest,
	})
	startTime := d.timeNow()

	var ve *manifest.VersionEdit
	var pendingOutputs []compactionOutput
	var stats compactStats
	// To determine the target level of the files in the ingestedFlushable, we
	// need to acquire the logLock, and not release it for that duration. Since,
	// we need to acquire the logLock below to perform the logAndApply step
	// anyway, we create the VersionEdit for ingestedFlushable outside of
	// runCompaction. For all other flush cases, we construct the VersionEdit
	// inside runCompaction.
	if c.kind != compactionKindIngestedFlushable {
		ve, pendingOutputs, stats, err = d.runCompaction(jobID, c)
	}

	// Acquire logLock. This will be released either on an error, by way of
	// logUnlock, or through a call to logAndApply if there is no error.
	d.mu.versions.logLock()

	if c.kind == compactionKindIngestedFlushable {
		ve, err = d.runIngestFlush(c)
	}

	info := FlushInfo{
		JobID:      int(jobID),
		Input:      inputs,
		InputBytes: inputBytes,
		Duration:   d.timeNow().Sub(startTime),
		Done:       true,
		Ingest:     ingest,
		Err:        err,
	}
	if err == nil {
		validateVersionEdit(ve, d.opts.Experimental.KeyValidationFunc, d.opts.Comparer.FormatKey, d.opts.Logger)
		for i := range ve.NewFiles {
			e := &ve.NewFiles[i]
			info.Output = append(info.Output, e.Meta.TableInfo())
			// Ingested tables are not necessarily flushed to L0. Record the level of
			// each ingested file explicitly.
			if ingest {
				info.IngestLevels = append(info.IngestLevels, e.Level)
			}
		}
		if len(ve.NewFiles) == 0 {
			info.Err = errEmptyTable
		}

		// The flush succeeded or it produced an empty sstable. In either case we
		// want to bump the minimum unflushed log number to the log number of the
		// oldest unflushed memtable.
		ve.MinUnflushedLogNum = minUnflushedLogNum
		if c.kind != compactionKindIngestedFlushable {
			metrics := c.metrics[0]
			if d.opts.DisableWAL {
				// If the WAL is disabled, every flushable has a zero [logSize],
				// resulting in zero bytes in. Instead, use the number of bytes we
				// flushed as the BytesIn. This ensures we get a reasonable w-amp
				// calculation even when the WAL is disabled.
				metrics.BytesIn = metrics.BytesFlushed
			} else {
				metrics := c.metrics[0]
				for i := 0; i < n; i++ {
					metrics.BytesIn += d.mu.mem.queue[i].logSize
				}
			}
		} else {
			// c.kind == compactionKindIngestedFlushable && we could have deleted files due
			// to ingest-time splits or excises.
			ingestFlushable := c.flushing[0].flushable.(*ingestedFlushable)
			for c2 := range d.mu.compact.inProgress {
				// Check if this compaction overlaps with the excise span. Note that just
				// checking if the inputs individually overlap with the excise span
				// isn't sufficient; for instance, a compaction could have [a,b] and [e,f]
				// as inputs and write it all out as [a,b,e,f] in one sstable. If we're
				// doing a [c,d) excise at the same time as this compaction, we will have
				// to error out the whole compaction as we can't guarantee it hasn't/won't
				// write a file overlapping with the excise span.
				if ingestFlushable.exciseSpan.OverlapsInternalKeyRange(d.cmp, c2.smallest, c2.largest) {
					c2.cancel.Store(true)
					continue
				}
			}

			if len(ve.DeletedFiles) > 0 {
				// Iterate through all other compactions, and check if their inputs have
				// been replaced due to an ingest-time split or excise. In that case,
				// cancel the compaction.
				for c2 := range d.mu.compact.inProgress {
					for i := range c2.inputs {
						iter := c2.inputs[i].files.Iter()
						for f := iter.First(); f != nil; f = iter.Next() {
							if _, ok := ve.DeletedFiles[deletedFileEntry{FileNum: f.FileNum, Level: c2.inputs[i].level}]; ok {
								c2.cancel.Store(true)
								break
							}
						}
					}
				}
			}
		}
		err = d.mu.versions.logAndApply(jobID, ve, c.metrics, false, /* forceRotation */
			func() []compactionInfo { return d.getInProgressCompactionInfoLocked(c) })
		if err != nil {
			info.Err = err
			// TODO(peter): untested.
			for _, f := range pendingOutputs {
				// Note that the FileBacking for the file metadata might not have
				// been set yet. So, we directly use the FileNum. Since these
				// files were generated as compaction outputs, these must be
				// physical files on disk. This property might not hold once
				// https://github.com/cockroachdb/pebble/issues/389 is
				// implemented if #389 creates virtual sstables as output files.
				d.mu.versions.obsoleteTables = append(d.mu.versions.obsoleteTables, tableInfo{
					fileInfo: fileInfo{
						FileNum:  base.PhysicalTableDiskFileNum(f.meta.FileNum),
						FileSize: f.meta.Size,
					},
					isLocal: f.isLocal,
				})
			}
			d.mu.versions.updateObsoleteTableMetricsLocked()
		}
	} else {
		// We won't be performing the logAndApply step because of the error,
		// so logUnlock.
		d.mu.versions.logUnlock()
	}

	// If err != nil, then the flush will be retried, and we will recalculate
	// these metrics.
	if err == nil {
		d.mu.snapshots.cumulativePinnedCount += stats.cumulativePinnedKeys
		d.mu.snapshots.cumulativePinnedSize += stats.cumulativePinnedSize
		d.mu.versions.metrics.Keys.MissizedTombstonesCount += stats.countMissizedDels
		d.maybeUpdateDeleteCompactionHints(c)
	}

	d.clearCompactingState(c, err != nil)
	delete(d.mu.compact.inProgress, c)
	d.mu.versions.incrementCompactions(c.kind, c.extraLevels, c.pickerMetrics)

	var flushed flushableList
	if err == nil {
		flushed = d.mu.mem.queue[:n]
		d.mu.mem.queue = d.mu.mem.queue[n:]
		d.updateReadStateLocked(d.opts.DebugCheck)
		d.updateTableStatsLocked(ve.NewFiles)
		if ingest {
			d.mu.versions.metrics.Flush.AsIngestCount++
			for _, l := range c.metrics {
				d.mu.versions.metrics.Flush.AsIngestBytes += l.BytesIngested
				d.mu.versions.metrics.Flush.AsIngestTableCount += l.TablesIngested
			}
		}
		d.maybeTransitionSnapshotsToFileOnlyLocked()

	}
	// Signal FlushEnd after installing the new readState. This helps for unit
	// tests that use the callback to trigger a read using an iterator with
	// IterOptions.OnlyReadGuaranteedDurable.
	info.TotalDuration = d.timeNow().Sub(startTime)
	d.opts.EventListener.FlushEnd(info)

	// The order of these operations matters here for ease of testing.
	// Removing the reader reference first allows tests to be guaranteed that
	// the memtable reservation has been released by the time a synchronous
	// flush returns. readerUnrefLocked may also produce obsolete files so the
	// call to deleteObsoleteFiles must happen after it.
	for i := range flushed {
		flushed[i].readerUnrefLocked(true)
	}

	d.deleteObsoleteFiles(jobID)

	// Mark all the memtables we flushed as flushed.
	for i := range flushed {
		close(flushed[i].flushed)
	}

	return inputBytes, err
}

// maybeTransitionSnapshotsToFileOnlyLocked transitions any "eventually
// file-only" snapshots to be file-only if all their visible state has been
// flushed to sstables.
//
// REQUIRES: d.mu.
func (d *DB) maybeTransitionSnapshotsToFileOnlyLocked() {
	earliestUnflushedSeqNum := d.getEarliestUnflushedSeqNumLocked()
	currentVersion := d.mu.versions.currentVersion()
	for s := d.mu.snapshots.root.next; s != &d.mu.snapshots.root; {
		if s.efos == nil {
			s = s.next
			continue
		}
		overlapsFlushable := false
		if base.Visible(earliestUnflushedSeqNum, s.efos.seqNum, InternalKeySeqNumMax) {
			// There are some unflushed keys that are still visible to the EFOS.
			// Check if any memtables older than the EFOS contain keys within a
			// protected range of the EFOS. If no, we can transition.
			protectedRanges := make([]bounded, len(s.efos.protectedRanges))
			for i := range s.efos.protectedRanges {
				protectedRanges[i] = s.efos.protectedRanges[i]
			}
			for i := range d.mu.mem.queue {
				if !base.Visible(d.mu.mem.queue[i].logSeqNum, s.efos.seqNum, InternalKeySeqNumMax) {
					// All keys in this memtable are newer than the EFOS. Skip this
					// memtable.
					continue
				}
				// NB: computePossibleOverlaps could have false positives, such as if
				// the flushable is a flushable ingest and not a memtable. In that
				// case we don't open the sstables to check; we just pessimistically
				// assume an overlap.
				d.mu.mem.queue[i].computePossibleOverlaps(func(b bounded) shouldContinue {
					overlapsFlushable = true
					return stopIteration
				}, protectedRanges...)
				if overlapsFlushable {
					break
				}
			}
		}
		if overlapsFlushable {
			s = s.next
			continue
		}
		currentVersion.Ref()

		// NB: s.efos.transitionToFileOnlySnapshot could close s, in which
		// case s.next would be nil. Save it before calling it.
		next := s.next
		_ = s.efos.transitionToFileOnlySnapshot(currentVersion)
		s = next
	}
}

// maybeScheduleCompactionAsync should be used when
// we want to possibly schedule a compaction, but don't
// want to eat the cost of running maybeScheduleCompaction.
// This method should be launched in a separate goroutine.
// d.mu must not be held when this is called.
func (d *DB) maybeScheduleCompactionAsync() {
	defer d.compactionSchedulers.Done()

	d.mu.Lock()
	d.maybeScheduleCompaction()
	d.mu.Unlock()
}

// maybeScheduleCompaction schedules a compaction if necessary.
//
// d.mu must be held when calling this.
func (d *DB) maybeScheduleCompaction() {
	d.maybeScheduleCompactionPicker(pickAuto)
}

func pickAuto(picker compactionPicker, env compactionEnv) *pickedCompaction {
	return picker.pickAuto(env)
}

func pickElisionOnly(picker compactionPicker, env compactionEnv) *pickedCompaction {
	return picker.pickElisionOnlyCompaction(env)
}

// tryScheduleDownloadCompaction tries to start a download compaction.
//
// Returns true if we started a download compaction (or completed it
// immediately because it is a no-op or we hit an error).
//
// Requires d.mu to be held. Updates d.mu.compact.downloads.
func (d *DB) tryScheduleDownloadCompaction(env compactionEnv, maxConcurrentDownloads int) bool {
	vers := d.mu.versions.currentVersion()
	for i := 0; i < len(d.mu.compact.downloads); {
		download := d.mu.compact.downloads[i]
		switch d.tryLaunchDownloadCompaction(download, vers, env, maxConcurrentDownloads) {
		case launchedCompaction:
			return true
		case didNotLaunchCompaction:
			// See if we can launch a compaction for another download task.
			i++
		case downloadTaskCompleted:
			// Task is completed and must be removed.
			d.mu.compact.downloads = slices.Delete(d.mu.compact.downloads, i, i+1)
		}
	}
	return false
}

// maybeScheduleCompactionPicker schedules a compaction if necessary,
// calling `pickFunc` to pick automatic compactions.
//
// Requires d.mu to be held.
func (d *DB) maybeScheduleCompactionPicker(
	pickFunc func(compactionPicker, compactionEnv) *pickedCompaction,
) {
	if d.closed.Load() != nil || d.opts.ReadOnly {
		return
	}
	maxCompactions := d.opts.MaxConcurrentCompactions()
	maxDownloads := d.opts.MaxConcurrentDownloads()

	if d.mu.compact.compactingCount >= maxCompactions &&
		(len(d.mu.compact.downloads) == 0 || d.mu.compact.downloadingCount >= maxDownloads) {
		if len(d.mu.compact.manual) > 0 {
			// Inability to run head blocks later manual compactions.
			d.mu.compact.manual[0].retries++
		}
		return
	}

	// Compaction picking needs a coherent view of a Version. In particular, we
	// need to exclude concurrent ingestions from making a decision on which level
	// to ingest into that conflicts with our compaction
	// decision. versionSet.logLock provides the necessary mutual exclusion.
	d.mu.versions.logLock()
	defer d.mu.versions.logUnlock()

	// Check for the closed flag again, in case the DB was closed while we were
	// waiting for logLock().
	if d.closed.Load() != nil {
		return
	}

	env := compactionEnv{
		diskAvailBytes:          d.diskAvailBytes.Load(),
		earliestSnapshotSeqNum:  d.mu.snapshots.earliest(),
		earliestUnflushedSeqNum: d.getEarliestUnflushedSeqNumLocked(),
	}

	if d.mu.compact.compactingCount < maxCompactions {
		// Check for delete-only compactions first, because they're expected to be
		// cheap and reduce future compaction work.
		if !d.opts.private.disableDeleteOnlyCompactions &&
			!d.opts.DisableAutomaticCompactions &&
			len(d.mu.compact.deletionHints) > 0 {
			d.tryScheduleDeleteOnlyCompaction()
		}

		for len(d.mu.compact.manual) > 0 && d.mu.compact.compactingCount < maxCompactions {
			if manual := d.mu.compact.manual[0]; !d.tryScheduleManualCompaction(env, manual) {
				// Inability to run head blocks later manual compactions.
				manual.retries++
				break
			}
			d.mu.compact.manual = d.mu.compact.manual[1:]
		}

		for !d.opts.DisableAutomaticCompactions && d.mu.compact.compactingCount < maxCompactions &&
			d.tryScheduleAutoCompaction(env, pickFunc) {
		}
	}

	for len(d.mu.compact.downloads) > 0 && d.mu.compact.downloadingCount < maxDownloads &&
		d.tryScheduleDownloadCompaction(env, maxDownloads) {
	}
}

// tryScheduleDeleteOnlyCompaction tries to kick off a delete-only compaction
// for all files that can be deleted as suggested by deletionHints.
//
// Requires d.mu to be held. Updates d.mu.compact.deletionHints.
func (d *DB) tryScheduleDeleteOnlyCompaction() {
	v := d.mu.versions.currentVersion()
	snapshots := d.mu.snapshots.toSlice()
	inputs, unresolvedHints := checkDeleteCompactionHints(d.cmp, v, d.mu.compact.deletionHints, snapshots)
	d.mu.compact.deletionHints = unresolvedHints

	if len(inputs) > 0 {
		c := newDeleteOnlyCompaction(d.opts, v, inputs, d.timeNow())
		d.mu.compact.compactingCount++
		d.addInProgressCompaction(c)
		go d.compact(c, nil)
	}
}

// tryScheduleManualCompaction tries to kick off the given manual compaction.
//
// Returns false if we are not able to run this compaction at this time.
//
// Requires d.mu to be held.
func (d *DB) tryScheduleManualCompaction(env compactionEnv, manual *manualCompaction) bool {
	v := d.mu.versions.currentVersion()
	env.inProgressCompactions = d.getInProgressCompactionInfoLocked(nil)
	pc, retryLater := pickManualCompaction(v, d.opts, env, d.mu.versions.picker.getBaseLevel(), manual)
	if pc == nil {
		if !retryLater {
			// Manual compaction is a no-op. Signal completion and exit.
			manual.done <- nil
			return true
		}
		// We are not able to run this manual compaction at this time.
		return false
	}

	c := newCompaction(pc, d.opts, d.timeNow(), d.ObjProvider())
	d.mu.compact.compactingCount++
	d.addInProgressCompaction(c)
	go d.compact(c, manual.done)
	return true
}

// tryScheduleAutoCompaction tries to kick off an automatic compaction.
//
// Returns false if no automatic compactions are necessary or able to run at
// this time.
//
// Requires d.mu to be held.
func (d *DB) tryScheduleAutoCompaction(
	env compactionEnv, pickFunc func(compactionPicker, compactionEnv) *pickedCompaction,
) bool {
	env.inProgressCompactions = d.getInProgressCompactionInfoLocked(nil)
	env.readCompactionEnv = readCompactionEnv{
		readCompactions:          &d.mu.compact.readCompactions,
		flushing:                 d.mu.compact.flushing || d.passedFlushThreshold(),
		rescheduleReadCompaction: &d.mu.compact.rescheduleReadCompaction,
	}
	pc := pickFunc(d.mu.versions.picker, env)
	if pc == nil {
		return false
	}
	c := newCompaction(pc, d.opts, d.timeNow(), d.ObjProvider())
	d.mu.compact.compactingCount++
	d.addInProgressCompaction(c)
	go d.compact(c, nil)
	return true
}

// deleteCompactionHintType indicates whether the deleteCompactionHint was
// generated from a span containing a range del (point key only), a range key
// delete (range key only), or both a point and range key.
type deleteCompactionHintType uint8

const (
	// NOTE: While these are primarily used as enumeration types, they are also
	// used for some bitwise operations. Care should be taken when updating.
	deleteCompactionHintTypeUnknown deleteCompactionHintType = iota
	deleteCompactionHintTypePointKeyOnly
	deleteCompactionHintTypeRangeKeyOnly
	deleteCompactionHintTypePointAndRangeKey
)

// String implements fmt.Stringer.
func (h deleteCompactionHintType) String() string {
	switch h {
	case deleteCompactionHintTypeUnknown:
		return "unknown"
	case deleteCompactionHintTypePointKeyOnly:
		return "point-key-only"
	case deleteCompactionHintTypeRangeKeyOnly:
		return "range-key-only"
	case deleteCompactionHintTypePointAndRangeKey:
		return "point-and-range-key"
	default:
		panic(fmt.Sprintf("unknown hint type: %d", h))
	}
}

// compactionHintFromKeys returns a deleteCompactionHintType given a slice of
// keyspan.Keys.
func compactionHintFromKeys(keys []keyspan.Key) deleteCompactionHintType {
	var hintType deleteCompactionHintType
	for _, k := range keys {
		switch k.Kind() {
		case base.InternalKeyKindRangeDelete:
			hintType |= deleteCompactionHintTypePointKeyOnly
		case base.InternalKeyKindRangeKeyDelete:
			hintType |= deleteCompactionHintTypeRangeKeyOnly
		default:
			panic(fmt.Sprintf("unsupported key kind: %s", k.Kind()))
		}
	}
	return hintType
}

// A deleteCompactionHint records a user key and sequence number span that has been
// deleted by a range tombstone. A hint is recorded if at least one sstable
// falls completely within both the user key and sequence number spans.
// Once the tombstones and the observed completely-contained sstables fall
// into the same snapshot stripe, a delete-only compaction may delete any
// sstables within the range.
type deleteCompactionHint struct {
	// The type of key span that generated this hint (point key, range key, or
	// both).
	hintType deleteCompactionHintType
	// start and end are user keys specifying a key range [start, end) of
	// deleted keys.
	start []byte
	end   []byte
	// The level of the file containing the range tombstone(s) when the hint
	// was created. Only lower levels need to be searched for files that may
	// be deleted.
	tombstoneLevel int
	// The file containing the range tombstone(s) that created the hint.
	tombstoneFile *fileMetadata
	// The smallest and largest sequence numbers of the abutting tombstones
	// merged to form this hint. All of a tables' keys must be less than the
	// tombstone smallest sequence number to be deleted. All of a tables'
	// sequence numbers must fall into the same snapshot stripe as the
	// tombstone largest sequence number to be deleted.
	tombstoneLargestSeqNum  uint64
	tombstoneSmallestSeqNum uint64
	// The smallest sequence number of a sstable that was found to be covered
	// by this hint. The hint cannot be resolved until this sequence number is
	// in the same snapshot stripe as the largest tombstone sequence number.
	// This is set when a hint is created, so the LSM may look different and
	// notably no longer contain the sstable that contained the key at this
	// sequence number.
	fileSmallestSeqNum uint64
}

func (h deleteCompactionHint) String() string {
	return fmt.Sprintf(
		"L%d.%s %s-%s seqnums(tombstone=%d-%d, file-smallest=%d, type=%s)",
		h.tombstoneLevel, h.tombstoneFile.FileNum, h.start, h.end,
		h.tombstoneSmallestSeqNum, h.tombstoneLargestSeqNum, h.fileSmallestSeqNum,
		h.hintType,
	)
}

func (h *deleteCompactionHint) canDelete(
	cmp Compare, m *fileMetadata, snapshots compact.Snapshots,
) bool {
	// The file can only be deleted if all of its keys are older than the
	// earliest tombstone aggregated into the hint.
	if m.LargestSeqNum >= h.tombstoneSmallestSeqNum || m.SmallestSeqNum < h.fileSmallestSeqNum {
		return false
	}

	// The file's oldest key must  be in the same snapshot stripe as the
	// newest tombstone. NB: We already checked the hint's sequence numbers,
	// but this file's oldest sequence number might be lower than the hint's
	// smallest sequence number despite the file falling within the key range
	// if this file was constructed after the hint by a compaction.
	if snapshots.Index(h.tombstoneLargestSeqNum) != snapshots.Index(m.SmallestSeqNum) {
		return false
	}

	switch h.hintType {
	case deleteCompactionHintTypePointKeyOnly:
		// A hint generated by a range del span cannot delete tables that contain
		// range keys.
		if m.HasRangeKeys {
			return false
		}
	case deleteCompactionHintTypeRangeKeyOnly:
		// A hint generated by a range key del span cannot delete tables that
		// contain point keys.
		if m.HasPointKeys {
			return false
		}
	case deleteCompactionHintTypePointAndRangeKey:
		// A hint from a span that contains both range dels *and* range keys can
		// only be deleted if both bounds fall within the hint. The next check takes
		// care of this.
	default:
		panic(fmt.Sprintf("pebble: unknown delete compaction hint type: %d", h.hintType))
	}

	// The file's keys must be completely contained within the hint range.
	return cmp(h.start, m.Smallest.UserKey) <= 0 && cmp(m.Largest.UserKey, h.end) < 0
}

func (d *DB) maybeUpdateDeleteCompactionHints(c *compaction) {
	// Compactions that zero sequence numbers can interfere with compaction
	// deletion hints. Deletion hints apply to tables containing keys older
	// than a threshold. If a key more recent than the threshold is zeroed in
	// a compaction, a delete-only compaction may mistake it as meeting the
	// threshold and drop a table containing live data.
	//
	// To avoid this scenario, compactions that zero sequence numbers remove
	// any conflicting deletion hints. A deletion hint is conflicting if both
	// of the following conditions apply:
	// * its key space overlaps with the compaction
	// * at least one of its inputs contains a key as recent as one of the
	//   hint's tombstones.
	//
	if !c.allowedZeroSeqNum {
		return
	}

	updatedHints := d.mu.compact.deletionHints[:0]
	for _, h := range d.mu.compact.deletionHints {
		// If the compaction's key space is disjoint from the hint's key
		// space, the zeroing of sequence numbers won't affect the hint. Keep
		// the hint.
		keysDisjoint := d.cmp(h.end, c.smallest.UserKey) < 0 || d.cmp(h.start, c.largest.UserKey) > 0
		if keysDisjoint {
			updatedHints = append(updatedHints, h)
			continue
		}

		// All of the compaction's inputs must be older than the hint's
		// tombstones.
		inputsOlder := true
		for _, in := range c.inputs {
			iter := in.files.Iter()
			for f := iter.First(); f != nil; f = iter.Next() {
				inputsOlder = inputsOlder && f.LargestSeqNum < h.tombstoneSmallestSeqNum
			}
		}
		if inputsOlder {
			updatedHints = append(updatedHints, h)
			continue
		}

		// Drop h, because the compaction c may have zeroed sequence numbers
		// of keys more recent than some of h's tombstones.
	}
	d.mu.compact.deletionHints = updatedHints
}

func checkDeleteCompactionHints(
	cmp Compare, v *version, hints []deleteCompactionHint, snapshots compact.Snapshots,
) ([]compactionLevel, []deleteCompactionHint) {
	var files map[*fileMetadata]bool
	var byLevel [numLevels][]*fileMetadata

	unresolvedHints := hints[:0]
	for _, h := range hints {
		// Check each compaction hint to see if it's resolvable. Resolvable
		// hints are removed and trigger a delete-only compaction if any files
		// in the current LSM still meet their criteria. Unresolvable hints
		// are saved and don't trigger a delete-only compaction.
		//
		// When a compaction hint is created, the sequence numbers of the
		// range tombstones and the covered file with the oldest key are
		// recorded. The largest tombstone sequence number and the smallest
		// file sequence number must be in the same snapshot stripe for the
		// hint to be resolved. The below graphic models a compaction hint
		// covering the keyspace [b, r). The hint completely contains two
		// files, 000002 and 000003. The file 000003 contains the lowest
		// covered sequence number at #90. The tombstone b.RANGEDEL.230:h has
		// the highest tombstone sequence number incorporated into the hint.
		// The hint may be resolved only once the snapshots at #100, #180 and
		// #210 are all closed. File 000001 is not included within the hint
		// because it extends beyond the range tombstones in user key space.
		//
		// 250
		//
		//       |-b...230:h-|
		// _____________________________________________________ snapshot #210
		// 200               |--h.RANGEDEL.200:r--|
		//
		// _____________________________________________________ snapshot #180
		//
		// 150                     +--------+
		//           +---------+   | 000003 |
		//           | 000002  |   |        |
		//           +_________+   |        |
		// 100_____________________|________|___________________ snapshot #100
		//                         +--------+
		// _____________________________________________________ snapshot #70
		//                             +---------------+
		//  50                         | 000001        |
		//                             |               |
		//                             +---------------+
		// ______________________________________________________________
		//     a b c d e f g h i j k l m n o p q r s t u v w x y z

		if snapshots.Index(h.tombstoneLargestSeqNum) != snapshots.Index(h.fileSmallestSeqNum) {
			// Cannot resolve yet.
			unresolvedHints = append(unresolvedHints, h)
			continue
		}

		// The hint h will be resolved and dropped, regardless of whether
		// there are any tables that can be deleted.
		for l := h.tombstoneLevel + 1; l < numLevels; l++ {
			overlaps := v.Overlaps(l, base.UserKeyBoundsEndExclusive(h.start, h.end))
			iter := overlaps.Iter()
			for m := iter.First(); m != nil; m = iter.Next() {
				if m.IsCompacting() || !h.canDelete(cmp, m, snapshots) || files[m] {
					continue
				}
				if files == nil {
					// Construct files lazily, assuming most calls will not
					// produce delete-only compactions.
					files = make(map[*fileMetadata]bool)
				}
				files[m] = true
				byLevel[l] = append(byLevel[l], m)
			}
		}
	}

	var compactLevels []compactionLevel
	for l, files := range byLevel {
		if len(files) == 0 {
			continue
		}
		compactLevels = append(compactLevels, compactionLevel{
			level: l,
			files: manifest.NewLevelSliceKeySorted(cmp, files),
		})
	}
	return compactLevels, unresolvedHints
}

// compact runs one compaction and maybe schedules another call to compact.
func (d *DB) compact(c *compaction, errChannel chan error) {
	pprof.Do(context.Background(), compactLabels, func(context.Context) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if err := d.compact1(c, errChannel); err != nil {
			// TODO(peter): count consecutive compaction errors and backoff.
			d.opts.EventListener.BackgroundError(err)
		}
		if c.isDownload {
			d.mu.compact.downloadingCount--
		} else {
			d.mu.compact.compactingCount--
		}
		delete(d.mu.compact.inProgress, c)
		// Add this compaction's duration to the cumulative duration. NB: This
		// must be atomic with the above removal of c from
		// d.mu.compact.InProgress to ensure Metrics.Compact.Duration does not
		// miss or double count a completing compaction's duration.
		d.mu.compact.duration += d.timeNow().Sub(c.beganAt)

		// The previous compaction may have produced too many files in a
		// level, so reschedule another compaction if needed.
		d.maybeScheduleCompaction()
		d.mu.compact.cond.Broadcast()
	})
}

// compact1 runs one compaction.
//
// d.mu must be held when calling this, but the mutex may be dropped and
// re-acquired during the course of this method.
func (d *DB) compact1(c *compaction, errChannel chan error) (err error) {
	if errChannel != nil {
		defer func() {
			errChannel <- err
		}()
	}

	jobID := d.newJobIDLocked()
	info := c.makeInfo(jobID)
	d.opts.EventListener.CompactionBegin(info)
	startTime := d.timeNow()

	ve, pendingOutputs, stats, err := d.runCompaction(jobID, c)

	info.Duration = d.timeNow().Sub(startTime)
	if err == nil {
		validateVersionEdit(ve, d.opts.Experimental.KeyValidationFunc, d.opts.Comparer.FormatKey, d.opts.Logger)
		err = func() error {
			var err error
			d.mu.versions.logLock()
			// Check if this compaction had a conflicting operation (eg. a d.excise())
			// that necessitates it restarting from scratch. Note that since we hold
			// the manifest lock, we don't expect this bool to change its value
			// as only the holder of the manifest lock will ever write to it.
			if c.cancel.Load() {
				err = firstError(err, ErrCancelledCompaction)
			}
			if err != nil {
				// logAndApply calls logUnlock. If we didn't call it, we need to call
				// logUnlock ourselves.
				d.mu.versions.logUnlock()
				return err
			}
			return d.mu.versions.logAndApply(jobID, ve, c.metrics, false /* forceRotation */, func() []compactionInfo {
				return d.getInProgressCompactionInfoLocked(c)
			})
		}()
		if err != nil {
			// TODO(peter): untested.
			for _, f := range pendingOutputs {
				// Note that the FileBacking for the file metadata might not have
				// been set yet. So, we directly use the FileNum. Since these
				// files were generated as compaction outputs, these must be
				// physical files on disk. This property might not hold once
				// https://github.com/cockroachdb/pebble/issues/389 is
				// implemented if #389 creates virtual sstables as output files.
				d.mu.versions.obsoleteTables = append(d.mu.versions.obsoleteTables, tableInfo{
					fileInfo: fileInfo{
						FileNum:  base.PhysicalTableDiskFileNum(f.meta.FileNum),
						FileSize: f.meta.Size,
					},
					isLocal: f.isLocal,
				})
			}
			d.mu.versions.updateObsoleteTableMetricsLocked()
		}
	}

	info.Done = true
	info.Err = err
	if err == nil {
		for i := range ve.NewFiles {
			e := &ve.NewFiles[i]
			info.Output.Tables = append(info.Output.Tables, e.Meta.TableInfo())
		}
		d.mu.snapshots.cumulativePinnedCount += stats.cumulativePinnedKeys
		d.mu.snapshots.cumulativePinnedSize += stats.cumulativePinnedSize
		d.mu.versions.metrics.Keys.MissizedTombstonesCount += stats.countMissizedDels
		d.maybeUpdateDeleteCompactionHints(c)
	}

	// NB: clearing compacting state must occur before updating the read state;
	// L0Sublevels initialization depends on it.
	d.clearCompactingState(c, err != nil)
	d.mu.versions.incrementCompactions(c.kind, c.extraLevels, c.pickerMetrics)
	d.mu.versions.incrementCompactionBytes(-c.bytesWritten)

	info.TotalDuration = d.timeNow().Sub(c.beganAt)
	d.opts.EventListener.CompactionEnd(info)

	// Update the read state before deleting obsolete files because the
	// read-state update will cause the previous version to be unref'd and if
	// there are no references obsolete tables will be added to the obsolete
	// table list.
	if err == nil {
		d.updateReadStateLocked(d.opts.DebugCheck)
		d.updateTableStatsLocked(ve.NewFiles)
	}
	d.deleteObsoleteFiles(jobID)

	return err
}

type compactStats struct {
	cumulativePinnedKeys uint64
	cumulativePinnedSize uint64
	countMissizedDels    uint64
}

// runCopyCompaction runs a copy compaction where a new FileNum is created that
// is a byte-for-byte copy of the input file or span thereof in some cases. This
// is used in lieu of a move compaction when a file is being moved across the
// local/remote storage boundary. It could also be used in lieu of a rewrite
// compaction as part of a Download() call, which allows copying only a span of
// the external file, provided the file does not contain range keys or value
// blocks (see sstable.CopySpan).
//
// d.mu must be held when calling this method. The mutex will be released when
// doing IO.
func (d *DB) runCopyCompaction(
	jobID JobID,
	c *compaction,
	inputMeta *fileMetadata,
	objMeta objstorage.ObjectMetadata,
	ve *versionEdit,
) (pendingOutputs []compactionOutput, retErr error) {
	ctx := context.TODO()

	if !objMeta.IsExternal() {
		if objMeta.IsRemote() || !remote.ShouldCreateShared(d.opts.Experimental.CreateOnShared, c.outputLevel.level) {
			panic("pebble: scheduled a copy compaction that is not actually moving files to shared storage")
		}
		// Note that based on logic in the compaction picker, we're guaranteed
		// inputMeta.Virtual is false.
		if inputMeta.Virtual {
			panic(errors.AssertionFailedf("cannot do a copy compaction of a virtual sstable across local/remote storage"))
		}
	}

	// We are in the relatively more complex case where we need to copy this
	// file to remote storage. Drop the db mutex while we do the copy
	//
	// To ease up cleanup of the local file and tracking of refs, we create
	// a new FileNum. This has the potential of making the block cache less
	// effective, however.
	newMeta := &fileMetadata{
		Size:            inputMeta.Size,
		CreationTime:    inputMeta.CreationTime,
		SmallestSeqNum:  inputMeta.SmallestSeqNum,
		LargestSeqNum:   inputMeta.LargestSeqNum,
		Stats:           inputMeta.Stats,
		Virtual:         inputMeta.Virtual,
		SyntheticPrefix: inputMeta.SyntheticPrefix,
		SyntheticSuffix: inputMeta.SyntheticSuffix,
	}
	if inputMeta.HasPointKeys {
		newMeta.ExtendPointKeyBounds(c.cmp, inputMeta.SmallestPointKey, inputMeta.LargestPointKey)
	}
	if inputMeta.HasRangeKeys {
		newMeta.ExtendRangeKeyBounds(c.cmp, inputMeta.SmallestRangeKey, inputMeta.LargestRangeKey)
	}
	newMeta.FileNum = d.mu.versions.getNextFileNum()
	if objMeta.IsExternal() {
		// external -> local/shared copy. File must be virtual.
		// We will update this size later after we produce the new backing file.
		newMeta.InitProviderBacking(base.DiskFileNum(newMeta.FileNum), inputMeta.FileBacking.Size)
	} else {
		// local -> shared copy. New file is guaranteed to not be virtual.
		newMeta.InitPhysicalBacking()
	}

	c.metrics = map[int]*LevelMetrics{
		c.outputLevel.level: {
			BytesIn:         inputMeta.Size,
			BytesCompacted:  inputMeta.Size,
			TablesCompacted: 1,
		},
	}

	// Before dropping the db mutex, grab a ref to the current version. This
	// prevents any concurrent excises from deleting files that this compaction
	// needs to read/maintain a reference to.
	vers := d.mu.versions.currentVersion()
	vers.Ref()
	defer vers.UnrefLocked()

	// NB: The order here is reversed, lock after unlock. This is similar to
	// runCompaction.
	d.mu.Unlock()
	defer d.mu.Lock()

	// If the src obj is external, we're doing an external to local/shared copy.
	if objMeta.IsExternal() {
		src, err := d.objProvider.OpenForReading(
			ctx, fileTypeTable, inputMeta.FileBacking.DiskFileNum, objstorage.OpenOptions{},
		)
		if err != nil {
			return pendingOutputs, err
		}
		defer func() {
			if src != nil {
				src.Close()
			}
		}()

		w, outObjMeta, err := d.objProvider.Create(
			ctx, fileTypeTable, base.PhysicalTableDiskFileNum(newMeta.FileNum),
			objstorage.CreateOptions{
				PreferSharedStorage: remote.ShouldCreateShared(d.opts.Experimental.CreateOnShared, c.outputLevel.level),
			},
		)
		if err != nil {
			return pendingOutputs, err
		}
		pendingOutputs = append(pendingOutputs, compactionOutput{
			meta:    newMeta,
			isLocal: !outObjMeta.IsRemote(),
		})

		start, end := newMeta.Smallest, newMeta.Largest
		if newMeta.SyntheticPrefix.IsSet() {
			start.UserKey = newMeta.SyntheticPrefix.Invert(start.UserKey)
			end.UserKey = newMeta.SyntheticPrefix.Invert(end.UserKey)
		}
		if newMeta.SyntheticSuffix.IsSet() {
			// Extend the bounds as necessary so that the keys don't include suffixes.
			start.UserKey = start.UserKey[:c.comparer.Split(start.UserKey)]
			if n := c.comparer.Split(end.UserKey); n < len(end.UserKey) {
				end = base.MakeRangeDeleteSentinelKey(c.comparer.ImmediateSuccessor(nil, end.UserKey[:n]))
			}
		}

		wrote, err := sstable.CopySpan(ctx,
			src, d.opts.MakeReaderOptions(),
			w, d.opts.MakeWriterOptions(c.outputLevel.level, d.FormatMajorVersion().MaxTableFormat()),
			start, end,
		)
		src = nil // We passed src to CopySpan; it's responsible for closing it.
		if err != nil {
			return pendingOutputs, err
		}
		newMeta.FileBacking.Size = wrote
		newMeta.Size = wrote
	} else {
		pendingOutputs = append(pendingOutputs, compactionOutput{
			meta:    newMeta.PhysicalMeta().FileMetadata,
			isLocal: true,
		})
		_, err := d.objProvider.LinkOrCopyFromLocal(context.TODO(), d.opts.FS,
			d.objProvider.Path(objMeta), fileTypeTable, newMeta.FileBacking.DiskFileNum,
			objstorage.CreateOptions{PreferSharedStorage: true})

		if err != nil {
			return pendingOutputs, err
		}
	}
	ve.NewFiles[0].Meta = newMeta
	if newMeta.Virtual {
		ve.CreatedBackingTables = []*fileBacking{newMeta.FileBacking}
	}

	if err := d.objProvider.Sync(); err != nil {
		return pendingOutputs, err
	}
	return pendingOutputs, nil
}

type compactionOutput struct {
	meta    *fileMetadata
	isLocal bool
}

func (d *DB) runDeleteOnlyCompaction(
	jobID JobID, c *compaction,
) (ve *versionEdit, pendingOutputs []compactionOutput, stats compactStats, retErr error) {
	c.metrics = make(map[int]*LevelMetrics, len(c.inputs))
	ve = &versionEdit{
		DeletedFiles: map[deletedFileEntry]*fileMetadata{},
	}
	for _, cl := range c.inputs {
		levelMetrics := &LevelMetrics{}
		iter := cl.files.Iter()
		for f := iter.First(); f != nil; f = iter.Next() {
			ve.DeletedFiles[deletedFileEntry{
				Level:   cl.level,
				FileNum: f.FileNum,
			}] = f
		}
		c.metrics[cl.level] = levelMetrics
	}
	return ve, nil, stats, nil
}

func (d *DB) runMoveOrCopyCompaction(
	jobID JobID, c *compaction,
) (ve *versionEdit, pendingOutputs []compactionOutput, stats compactStats, _ error) {
	iter := c.startLevel.files.Iter()
	meta := iter.First()
	if invariants.Enabled {
		if iter.Next() != nil {
			panic("got more than one file for a move or copy compaction")
		}
	}
	if c.cancel.Load() {
		return ve, nil, stats, ErrCancelledCompaction
	}
	objMeta, err := d.objProvider.Lookup(fileTypeTable, meta.FileBacking.DiskFileNum)
	if err != nil {
		return ve, pendingOutputs, stats, err
	}
	c.metrics = map[int]*LevelMetrics{
		c.outputLevel.level: {
			BytesMoved:  meta.Size,
			TablesMoved: 1,
		},
	}
	ve = &versionEdit{
		DeletedFiles: map[deletedFileEntry]*fileMetadata{
			{Level: c.startLevel.level, FileNum: meta.FileNum}: meta,
		},
		NewFiles: []newFileEntry{
			{Level: c.outputLevel.level, Meta: meta},
		},
	}
	if c.kind == compactionKindMove {
		return ve, nil, stats, nil
	}

	pendingOutputs, err = d.runCopyCompaction(jobID, c, meta, objMeta, ve)
	return ve, pendingOutputs, stats, err
}

// runCompaction runs a compaction that produces new on-disk tables from
// memtables or old on-disk tables.
//
// runCompaction cannot be used for compactionKindIngestedFlushable.
//
// d.mu must be held when calling this, but the mutex may be dropped and
// re-acquired during the course of this method.
func (d *DB) runCompaction(
	jobID JobID, c *compaction,
) (ve *versionEdit, pendingOutputs []compactionOutput, stats compactStats, retErr error) {
	switch c.kind {
	case compactionKindDeleteOnly:
		return d.runDeleteOnlyCompaction(jobID, c)
	case compactionKindMove, compactionKindCopy:
		return d.runMoveOrCopyCompaction(jobID, c)
	case compactionKindIngestedFlushable:
		panic("pebble: runCompaction cannot handle compactionKindIngestedFlushable.")
	}

	defer func() {
		if retErr != nil {
			pendingOutputs = nil
		}
	}()

	snapshots := d.mu.snapshots.toSlice()
	formatVers := d.FormatMajorVersion()

	if c.flushing == nil {
		// Before dropping the db mutex, grab a ref to the current version. This
		// prevents any concurrent excises from deleting files that this compaction
		// needs to read/maintain a reference to.
		//
		// Note that unlike user iterators, compactionIter does not maintain a ref
		// of the version or read state.
		vers := d.mu.versions.currentVersion()
		vers.Ref()
		defer vers.UnrefLocked()
	}

	if c.cancel.Load() {
		return ve, nil, stats, ErrCancelledCompaction
	}

	// Release the d.mu lock while doing I/O.
	// Note the unusual order: Unlock and then Lock.
	d.mu.Unlock()
	defer d.mu.Lock()

	// Compactions use a pool of buffers to read blocks, avoiding polluting the
	// block cache with blocks that will not be read again. We initialize the
	// buffer pool with a size 12. This initial size does not need to be
	// accurate, because the pool will grow to accommodate the maximum number of
	// blocks allocated at a given time over the course of the compaction. But
	// choosing a size larger than that working set avoids any additional
	// allocations to grow the size of the pool over the course of iteration.
	//
	// Justification for initial size 12: In a two-level compaction, at any
	// given moment we'll have 2 index blocks in-use and 2 data blocks in-use.
	// Additionally, when decoding a compressed block, we'll temporarily
	// allocate 1 additional block to hold the compressed buffer. In the worst
	// case that all input sstables have two-level index blocks (+2), value
	// blocks (+2), range deletion blocks (+n) and range key blocks (+n), we'll
	// additionally require 2n+4 blocks where n is the number of input sstables.
	// Range deletion and range key blocks are relatively rare, and the cost of
	// an additional allocation or two over the course of the compaction is
	// considered to be okay. A larger initial size would cause the pool to hold
	// on to more memory, even when it's not in-use because the pool will
	// recycle buffers up to the current capacity of the pool. The memory use of
	// a 12-buffer pool is expected to be within reason, even if all the buffers
	// grow to the typical size of an index block (256 KiB) which would
	// translate to 3 MiB per compaction.
	c.bufferPool.Init(12)
	defer c.bufferPool.Release()

	iiter, err := c.newInputIter(d.newIters, d.tableNewRangeKeyIter, snapshots)
	if err != nil {
		return nil, pendingOutputs, stats, err
	}
	c.allowedZeroSeqNum = c.allowZeroSeqNum()
	iiter = invalidating.MaybeWrapIfInvariants(iiter)
	cfg := compact.IterConfig{
		Cmp:                                    c.cmp,
		Equal:                                  c.equal,
		Merge:                                  d.merge,
		Snapshots:                              snapshots,
		AllowZeroSeqNum:                        c.allowedZeroSeqNum,
		ElideTombstone:                         c.elideTombstone,
		ElideRangeTombstone:                    c.elideRangeTombstone,
		IneffectualSingleDeleteCallback:        d.opts.Experimental.IneffectualSingleDeleteCallback,
		SingleDeleteInvariantViolationCallback: d.opts.Experimental.SingleDeleteInvariantViolationCallback,
	}
	iter := compact.NewIter(cfg, iiter)

	var (
		createdFiles    []base.DiskFileNum
		tw              *sstable.Writer
		pinnedKeySize   uint64
		pinnedValueSize uint64
		pinnedCount     uint64
	)
	defer func() {
		if iter != nil {
			retErr = firstError(retErr, iter.Close())
		}
		if tw != nil {
			retErr = firstError(retErr, tw.Close())
		}
		if retErr != nil {
			for _, fileNum := range createdFiles {
				_ = d.objProvider.Remove(fileTypeTable, fileNum)
			}
		}
		for _, closer := range c.closers {
			retErr = firstError(retErr, closer.Close())
		}
	}()

	ve = &versionEdit{
		DeletedFiles: map[deletedFileEntry]*fileMetadata{},
	}

	startLevelBytes := c.startLevel.files.SizeSum()
	outputMetrics := &LevelMetrics{
		BytesIn:   startLevelBytes,
		BytesRead: c.outputLevel.files.SizeSum(),
	}
	if len(c.extraLevels) > 0 {
		outputMetrics.BytesIn += c.extraLevels[0].files.SizeSum()
	}
	outputMetrics.BytesRead += outputMetrics.BytesIn

	c.metrics = map[int]*LevelMetrics{
		c.outputLevel.level: outputMetrics,
	}
	if len(c.flushing) == 0 && c.metrics[c.startLevel.level] == nil {
		c.metrics[c.startLevel.level] = &LevelMetrics{}
	}
	if len(c.extraLevels) > 0 {
		c.metrics[c.extraLevels[0].level] = &LevelMetrics{}
		outputMetrics.MultiLevel.BytesInTop = startLevelBytes
		outputMetrics.MultiLevel.BytesIn = outputMetrics.BytesIn
		outputMetrics.MultiLevel.BytesRead = outputMetrics.BytesRead
	}

	// The table is typically written at the maximum allowable format implied by
	// the current format major version of the DB.
	tableFormat := formatVers.MaxTableFormat()

	// In format major versions with maximum table formats of Pebblev3, value
	// blocks were conditional on an experimental setting. In format major
	// versions with maximum table formats of Pebblev4 and higher, value blocks
	// are always enabled.
	if tableFormat == sstable.TableFormatPebblev3 &&
		(d.opts.Experimental.EnableValueBlocks == nil || !d.opts.Experimental.EnableValueBlocks()) {
		tableFormat = sstable.TableFormatPebblev2
	}

	writerOpts := d.opts.MakeWriterOptions(c.outputLevel.level, tableFormat)

	// prevPointKey is a sstable.WriterOption that provides access to
	// the last point key written to a writer's sstable. When a new
	// output begins in newOutput, prevPointKey is updated to point to
	// the new output's sstable.Writer. This allows the compaction loop
	// to access the last written point key without requiring the
	// compaction loop to make a copy of each key ahead of time. Users
	// must be careful, because the byte slice returned by UnsafeKey
	// points directly into the Writer's block buffer.
	var prevPointKey sstable.PreviousPointKeyOpt
	var cpuWorkHandle CPUWorkHandle
	defer func() {
		if cpuWorkHandle != nil {
			d.opts.Experimental.CPUWorkPermissionGranter.CPUWorkDone(cpuWorkHandle)
		}
	}()

	newOutput := func() error {
		// Check if we've been cancelled by a concurrent operation.
		if c.cancel.Load() {
			return ErrCancelledCompaction
		}
		d.mu.Lock()
		fileNum := d.mu.versions.getNextFileNum()
		d.mu.Unlock()

		ctx := context.TODO()
		if objiotracing.Enabled {
			ctx = objiotracing.WithLevel(ctx, c.outputLevel.level)
			switch c.kind {
			case compactionKindFlush:
				ctx = objiotracing.WithReason(ctx, objiotracing.ForFlush)
			default:
				ctx = objiotracing.WithReason(ctx, objiotracing.ForCompaction)
			}
		}
		var writeCategory vfs.DiskWriteCategory
		switch c.kind {
		case compactionKindFlush:
			if d.opts.EnableSQLRowSpillMetrics {
				// In the scenario that the Pebble engine is used for SQL row spills the data written to
				// the memtable will correspond to spills to disk and should be categorized as such.
				writeCategory = "sql-row-spill"
			} else {
				writeCategory = "pebble-memtable-flush"
			}
		default:
			writeCategory = "pebble-compaction"
		}
		// Prefer shared storage if present.
		createOpts := objstorage.CreateOptions{
			PreferSharedStorage: remote.ShouldCreateShared(d.opts.Experimental.CreateOnShared, c.outputLevel.level),
			WriteCategory:       writeCategory,
		}
		diskFileNum := base.PhysicalTableDiskFileNum(fileNum)
		writable, objMeta, err := d.objProvider.Create(ctx, fileTypeTable, diskFileNum, createOpts)
		if err != nil {
			return err
		}
		fileMeta := &fileMetadata{}
		fileMeta.FileNum = fileNum
		pendingOutputs = append(pendingOutputs, compactionOutput{
			meta:    fileMeta.PhysicalMeta().FileMetadata,
			isLocal: !objMeta.IsRemote(),
		})

		reason := "flushing"
		if c.flushing == nil {
			reason = "compacting"
		}
		d.opts.EventListener.TableCreated(TableCreateInfo{
			JobID:   int(jobID),
			Reason:  reason,
			Path:    d.objProvider.Path(objMeta),
			FileNum: diskFileNum,
		})
		if c.kind != compactionKindFlush {
			writable = &compactionWritable{
				Writable: writable,
				versions: d.mu.versions,
				written:  &c.bytesWritten,
			}
		}
		createdFiles = append(createdFiles, diskFileNum)
		cacheOpts := private.SSTableCacheOpts(d.cacheID, diskFileNum).(sstable.WriterOption)

		const MaxFileWriteAdditionalCPUTime = time.Millisecond * 100
		cpuWorkHandle = d.opts.Experimental.CPUWorkPermissionGranter.GetPermission(
			MaxFileWriteAdditionalCPUTime,
		)
		writerOpts.Parallelism =
			d.opts.Experimental.MaxWriterConcurrency > 0 &&
				(cpuWorkHandle.Permitted() || d.opts.Experimental.ForceWriterParallelism)

		tw = sstable.NewWriter(writable, writerOpts, cacheOpts, &prevPointKey)

		fileMeta.CreationTime = time.Now().Unix()
		ve.NewFiles = append(ve.NewFiles, newFileEntry{
			Level: c.outputLevel.level,
			Meta:  fileMeta,
		})
		return nil
	}

	// splitL0Outputs is true during flushes and intra-L0 compactions with flush
	// splits enabled.
	splitL0Outputs := c.outputLevel.level == 0 && d.opts.FlushSplitBytes > 0

	// finishOutput is called with the a user key up to which all tombstones
	// should be flushed. Typically, this is the first key of the next
	// sstable or an empty key if this output is the final sstable.
	finishOutput := func(splitKey []byte) error {
		// If we haven't output any point records to the sstable (tw == nil) then the
		// sstable will only contain range tombstones and/or range keys. The smallest
		// key in the sstable will be the start key of the first range tombstone or
		// range key added. We need to ensure that this start key is distinct from
		// the splitKey passed to finishOutput (if set), otherwise we would generate
		// an sstable where the largest key is smaller than the smallest key due to
		// how the largest key boundary is set below. NB: It is permissible for the
		// range tombstone / range key start key to be nil.
		//
		// TODO: It is unfortunate that we have to do this check here rather than
		// when we decide to finish the sstable in the runCompaction loop. A better
		// structure currently eludes us.
		if tw == nil {
			startKey := iter.FirstTombstoneStart()
			if startKey == nil {
				startKey = iter.FirstRangeKeyStart()
			}
			if splitKey != nil && d.cmp(startKey, splitKey) == 0 {
				return nil
			}
		}

		for _, v := range iter.TombstonesUpTo(splitKey) {
			if tw == nil {
				if err := newOutput(); err != nil {
					return err
				}
			}
			// The tombstone being added could be completely outside the
			// eventual bounds of the sstable. Consider this example (bounds
			// in square brackets next to table filename):
			//
			// ./000240.sst   [tmgc#391,MERGE-tmgc#391,MERGE]
			// tmgc#391,MERGE [786e627a]
			// tmgc-udkatvs#331,RANGEDEL
			//
			// ./000241.sst   [tmgc#384,MERGE-tmgc#384,MERGE]
			// tmgc#384,MERGE [666c7070]
			// tmgc-tvsalezade#383,RANGEDEL
			// tmgc-tvsalezade#331,RANGEDEL
			//
			// ./000242.sst   [tmgc#383,RANGEDEL-tvsalezade#72057594037927935,RANGEDEL]
			// tmgc-tvsalezade#383,RANGEDEL
			// tmgc#375,SET [72646c78766965616c72776865676e79]
			// tmgc-tvsalezade#356,RANGEDEL
			//
			// Note that both of the top two SSTables have range tombstones
			// that start after the file's end keys. Since the file bound
			// computation happens well after all range tombstones have been
			// added to the writer, eliding out-of-file range tombstones based
			// on sequence number at this stage is difficult, and necessitates
			// read-time logic to ignore range tombstones outside file bounds.
			if err := rangedel.Encode(&v, tw.Add); err != nil {
				return err
			}
		}
		for _, v := range iter.RangeKeysUpTo(splitKey) {
			// Same logic as for range tombstones, except added using tw.AddRangeKey.
			if tw == nil {
				if err := newOutput(); err != nil {
					return err
				}
			}
			if err := rangekey.Encode(&v, tw.AddRangeKey); err != nil {
				return err
			}
		}

		if tw == nil {
			return nil
		}
		{
			// Set internal sstable properties.
			p := getInternalWriterProperties(tw)
			// Set the snapshot pinned totals.
			p.SnapshotPinnedKeys = pinnedCount
			p.SnapshotPinnedKeySize = pinnedKeySize
			p.SnapshotPinnedValueSize = pinnedValueSize
			stats.cumulativePinnedKeys += pinnedCount
			stats.cumulativePinnedSize += pinnedKeySize + pinnedValueSize
			pinnedCount = 0
			pinnedKeySize = 0
			pinnedValueSize = 0
		}
		if err := tw.Close(); err != nil {
			tw = nil
			return err
		}
		d.opts.Experimental.CPUWorkPermissionGranter.CPUWorkDone(cpuWorkHandle)
		cpuWorkHandle = nil
		writerMeta, err := tw.Metadata()
		if err != nil {
			tw = nil
			return err
		}
		tw = nil
		meta := ve.NewFiles[len(ve.NewFiles)-1].Meta
		meta.Size = writerMeta.Size
		meta.SmallestSeqNum = writerMeta.SmallestSeqNum
		meta.LargestSeqNum = writerMeta.LargestSeqNum
		meta.InitPhysicalBacking()

		// If the file didn't contain any range deletions, we can fill its
		// table stats now, avoiding unnecessarily loading the table later.
		maybeSetStatsFromProperties(
			meta.PhysicalMeta(), &writerMeta.Properties,
		)

		if c.flushing == nil {
			outputMetrics.TablesCompacted++
			outputMetrics.BytesCompacted += meta.Size
		} else {
			outputMetrics.TablesFlushed++
			outputMetrics.BytesFlushed += meta.Size
		}
		outputMetrics.Size += int64(meta.Size)
		outputMetrics.NumFiles++
		outputMetrics.Additional.BytesWrittenDataBlocks += writerMeta.Properties.DataSize
		outputMetrics.Additional.BytesWrittenValueBlocks += writerMeta.Properties.ValueBlocksSize

		if n := len(ve.NewFiles); n > 1 {
			// This is not the first output file. Ensure the sstable boundaries
			// are nonoverlapping.
			prevMeta := ve.NewFiles[n-2].Meta
			if writerMeta.SmallestRangeDel.UserKey != nil {
				c := d.cmp(writerMeta.SmallestRangeDel.UserKey, prevMeta.Largest.UserKey)
				if c < 0 {
					return errors.Errorf(
						"pebble: smallest range tombstone start key is less than previous sstable largest key: %s < %s",
						writerMeta.SmallestRangeDel.Pretty(d.opts.Comparer.FormatKey),
						prevMeta.Largest.Pretty(d.opts.Comparer.FormatKey))
				} else if c == 0 && !prevMeta.Largest.IsExclusiveSentinel() {
					// The user key portion of the range boundary start key is
					// equal to the previous table's largest key user key, and
					// the previous table's largest key is not exclusive. This
					// violates the invariant that tables are key-space
					// partitioned.
					return errors.Errorf(
						"pebble: invariant violation: previous sstable largest key %s, current sstable smallest rangedel: %s",
						prevMeta.Largest.Pretty(d.opts.Comparer.FormatKey),
						writerMeta.SmallestRangeDel.Pretty(d.opts.Comparer.FormatKey),
					)
				}
			}
		}

		// Verify that all range deletions outputted to the sstable are
		// truncated to split key.
		if splitKey != nil && writerMeta.LargestRangeDel.UserKey != nil &&
			d.cmp(writerMeta.LargestRangeDel.UserKey, splitKey) > 0 {
			return errors.Errorf(
				"pebble: invariant violation: rangedel largest key %q extends beyond split key %q",
				writerMeta.LargestRangeDel.Pretty(d.opts.Comparer.FormatKey),
				d.opts.Comparer.FormatKey(splitKey),
			)
		}

		if writerMeta.HasPointKeys {
			meta.ExtendPointKeyBounds(d.cmp, writerMeta.SmallestPoint, writerMeta.LargestPoint)
		}
		if writerMeta.HasRangeDelKeys {
			meta.ExtendPointKeyBounds(d.cmp, writerMeta.SmallestRangeDel, writerMeta.LargestRangeDel)
		}
		if writerMeta.HasRangeKeys {
			meta.ExtendRangeKeyBounds(d.cmp, writerMeta.SmallestRangeKey, writerMeta.LargestRangeKey)
		}

		// Verify that the sstable bounds fall within the compaction input
		// bounds. This is a sanity check that we don't have a logic error
		// elsewhere that causes the sstable bounds to accidentally expand past the
		// compaction input bounds as doing so could lead to various badness such
		// as keys being deleted by a range tombstone incorrectly.
		if c.smallest.UserKey != nil {
			switch v := d.cmp(meta.Smallest.UserKey, c.smallest.UserKey); {
			case v >= 0:
				// Nothing to do.
			case v < 0:
				return errors.Errorf("pebble: compaction output grew beyond bounds of input: %s < %s",
					meta.Smallest.Pretty(d.opts.Comparer.FormatKey),
					c.smallest.Pretty(d.opts.Comparer.FormatKey))
			}
		}
		if c.largest.UserKey != nil {
			switch v := d.cmp(meta.Largest.UserKey, c.largest.UserKey); {
			case v <= 0:
				// Nothing to do.
			case v > 0:
				return errors.Errorf("pebble: compaction output grew beyond bounds of input: %s > %s",
					meta.Largest.Pretty(d.opts.Comparer.FormatKey),
					c.largest.Pretty(d.opts.Comparer.FormatKey))
			}
		}
		// Verify that we never split different revisions of the same user key
		// across two different sstables.
		if err := c.errorOnUserKeyOverlap(ve); err != nil {
			return err
		}
		if err := meta.Validate(d.cmp, d.opts.Comparer.FormatKey); err != nil {
			return err
		}
		return nil
	}

	// Build a compactionOutputSplitter that contains all logic to determine
	// whether the compaction loop should stop writing to one output sstable and
	// switch to a new one. Some splitters can wrap other splitters, and the
	// splitterGroup can be composed of multiple splitters. In this case, we
	// start off with splitters for file sizes, grandparent limits, and (for L0
	// splits) L0 limits, before wrapping them in an splitterGroup.
	unsafePrevUserKey := func() []byte {
		// Return the largest point key written to tw or the start of
		// the current range deletion in the fragmenter, whichever is
		// greater.
		prevPoint := prevPointKey.UnsafeKey()
		if c.cmp(prevPoint.UserKey, iter.FirstTombstoneStart()) > 0 {
			return prevPoint.UserKey
		}
		return iter.FirstTombstoneStart()
	}
	outputSplitters := []compact.OutputSplitter{
		// We do not split the same user key across different sstables within
		// one flush or compaction. The FileSizeSplitter may request a split in
		// the middle of a user key, so PreventSplitUserKeys ensures we are at a
		// user key change boundary when doing a split.
		compact.PreventSplitUserKeys(
			c.cmp,
			compact.FileSizeSplitter(iter.Frontiers(), c.maxOutputFileSize, c.grandparents.Iter()),
			unsafePrevUserKey,
		),
		compact.LimitFuncSplitter(iter.Frontiers(), c.findGrandparentLimit),
	}
	if splitL0Outputs {
		outputSplitters = append(outputSplitters, compact.LimitFuncSplitter(iter.Frontiers(), c.findL0Limit))
	}
	splitter := compact.CombineSplitters(c.cmp, outputSplitters...)

	// Each outer loop iteration produces one output file. An iteration that
	// produces a file containing point keys (and optionally range tombstones)
	// guarantees that the input iterator advanced. An iteration that produces
	// a file containing only range tombstones guarantees the limit passed to
	// `finishOutput()` advanced to a strictly greater user key corresponding
	// to a grandparent file largest key, or nil. Taken together, these
	// progress guarantees ensure that eventually the input iterator will be
	// exhausted and the range tombstone fragments will all be flushed.
	for key, val := iter.First(); key != nil || iter.FirstTombstoneStart() != nil || iter.FirstRangeKeyStart() != nil; {
		var firstKey []byte
		if key != nil {
			firstKey = key.UserKey
		} else if startKey := iter.FirstTombstoneStart(); startKey != nil {
			// Pass the start key of the first pending tombstone to find the
			// next limit. All pending tombstones have the same start key. We
			// use this as opposed to the end key of the last written sstable to
			// effectively handle cases like these:
			//
			// a.SET.3
			// (lf.limit at b)
			// d.RANGEDEL.4:f
			//
			// In this case, the partition after b has only range deletions, so
			// if we were to find the limit after the last written key at the
			// split point (key a), we'd get the limit b again, and
			// finishOutput() would not advance any further because the next
			// range tombstone to write does not start until after the L0 split
			// point.
			firstKey = startKey
		}
		splitterSuggestion := splitter.OnNewOutput(firstKey)

		// Each inner loop iteration processes one key from the input iterator.
		for ; key != nil; key, val = iter.Next() {
			if split := splitter.ShouldSplitBefore(key, tw); split == compact.SplitNow {
				break
			}

			switch key.Kind() {
			case InternalKeyKindRangeDelete:
				// Range tombstones are handled specially. They are not written until
				// later during `finishOutput()`. We add them to the compaction iterator
				// so covered keys in the same snapshot stripe can be elided.
				//
				// Since the keys' Suffix and Value fields are not deep cloned, the
				// underlying blockIter must be kept open for the lifetime of the
				// compaction.
				iter.AddTombstoneSpan(c.rangeDelInterleaving.Span())
				continue
			case InternalKeyKindRangeKeySet, InternalKeyKindRangeKeyUnset, InternalKeyKindRangeKeyDelete:
				// Range keys are handled in the same way as range tombstones.
				// Since the keys' Suffix and Value fields are not deep cloned, the
				// underlying blockIter must be kept open for the lifetime of the
				// compaction.
				iter.AddRangeKeySpan(c.rangeKeyInterleaving.Span())
				continue
			}
			if tw == nil {
				if err := newOutput(); err != nil {
					return nil, pendingOutputs, stats, err
				}
			}
			if err := tw.AddWithForceObsolete(*key, val, iter.ForceObsoleteDueToRangeDel()); err != nil {
				return nil, pendingOutputs, stats, err
			}
			if iter.SnapshotPinned() {
				// The kv pair we just added to the sstable was only surfaced by
				// the compaction iterator because an open snapshot prevented
				// its elision. Increment the stats.
				pinnedCount++
				pinnedKeySize += uint64(len(key.UserKey)) + base.InternalTrailerLen
				pinnedValueSize += uint64(len(val))
			}
		}

		// A splitter requested a split, and we're ready to finish the output.
		// We need to choose the key at which to split any pending range
		// tombstones. There are two options:
		// 1. splitterSuggestion — The key suggested by the splitter. This key
		//    is guaranteed to be greater than the last key written to the
		//    current output.
		// 2. key.UserKey — the first key of the next sstable output. This user
		//     key is also guaranteed to be greater than the last user key
		//     written to the current output (see userKeyChangeSplitter).
		//
		// Use whichever is smaller. Using the smaller of the two limits
		// overlap with grandparents. Consider the case where the
		// grandparent limit is calculated to be 'b', key is 'x', and
		// there exist many sstables between 'b' and 'x'. If the range
		// deletion fragmenter has a pending tombstone [a,x), splitting
		// at 'x' would cause the output table to overlap many
		// grandparents well beyond the calculated grandparent limit
		// 'b'. Splitting at the smaller `splitterSuggestion` avoids
		// this unbounded overlap with grandparent tables.
		splitKey := splitterSuggestion
		if key != nil && (splitKey == nil || c.cmp(splitKey, key.UserKey) > 0) {
			splitKey = key.UserKey
		}
		if err := finishOutput(splitKey); err != nil {
			return nil, pendingOutputs, stats, err
		}
	}

	for _, cl := range c.inputs {
		iter := cl.files.Iter()
		for f := iter.First(); f != nil; f = iter.Next() {
			ve.DeletedFiles[deletedFileEntry{
				Level:   cl.level,
				FileNum: f.FileNum,
			}] = f
		}
	}

	// The compaction iterator keeps track of a count of the number of DELSIZED
	// keys that encoded an incorrect size. Propagate it up as a part of
	// compactStats.
	stats.countMissizedDels = iter.Stats().CountMissizedDels

	if err := d.objProvider.Sync(); err != nil {
		return nil, pendingOutputs, stats, err
	}

	// Refresh the disk available statistic whenever a compaction/flush
	// completes, before re-acquiring the mutex.
	_ = d.calculateDiskAvailableBytes()

	return ve, pendingOutputs, stats, nil
}

// validateVersionEdit validates that start and end keys across new and deleted
// files in a versionEdit pass the given validation function.
func validateVersionEdit(
	ve *versionEdit, validateFn func([]byte) error, format base.FormatKey, logger Logger,
) {
	validateKey := func(f *manifest.FileMetadata, key []byte) {
		if err := validateFn(key); err != nil {
			logger.Fatalf("pebble: version edit validation failed (key=%s file=%s): %v", format(key), f, err)
		}
	}

	// Validate both new and deleted files.
	for _, f := range ve.NewFiles {
		validateKey(f.Meta, f.Meta.Smallest.UserKey)
		validateKey(f.Meta, f.Meta.Largest.UserKey)
	}
	for _, m := range ve.DeletedFiles {
		validateKey(m, m.Smallest.UserKey)
		validateKey(m, m.Largest.UserKey)
	}
}

// scanObsoleteFiles scans the filesystem for files that are no longer needed
// and adds those to the internal lists of obsolete files. Note that the files
// are not actually deleted by this method. A subsequent call to
// deleteObsoleteFiles must be performed. Must be not be called concurrently
// with compactions and flushes. db.mu must be held when calling this function.
func (d *DB) scanObsoleteFiles(list []string) {
	// Disable automatic compactions temporarily to avoid concurrent compactions /
	// flushes from interfering. The original value is restored on completion.
	disabledPrev := d.opts.DisableAutomaticCompactions
	defer func() {
		d.opts.DisableAutomaticCompactions = disabledPrev
	}()
	d.opts.DisableAutomaticCompactions = true

	// Wait for any ongoing compaction to complete before continuing.
	for d.mu.compact.compactingCount > 0 || d.mu.compact.downloadingCount > 0 || d.mu.compact.flushing {
		d.mu.compact.cond.Wait()
	}

	liveFileNums := make(map[base.DiskFileNum]struct{})
	d.mu.versions.addLiveFileNums(liveFileNums)
	// Protect against files which are only referred to by the ingestedFlushable
	// from being deleted. These are added to the flushable queue on WAL replay
	// during read only mode and aren't part of the Version. Note that if
	// !d.opts.ReadOnly, then all flushables of type ingestedFlushable have
	// already been flushed.
	for _, fEntry := range d.mu.mem.queue {
		if f, ok := fEntry.flushable.(*ingestedFlushable); ok {
			for _, file := range f.files {
				liveFileNums[file.FileBacking.DiskFileNum] = struct{}{}
			}
		}
	}

	manifestFileNum := d.mu.versions.manifestFileNum

	var obsoleteTables []tableInfo
	var obsoleteManifests []fileInfo
	var obsoleteOptions []fileInfo

	for _, filename := range list {
		fileType, diskFileNum, ok := base.ParseFilename(d.opts.FS, filename)
		if !ok {
			continue
		}
		switch fileType {
		case fileTypeManifest:
			if diskFileNum >= manifestFileNum {
				continue
			}
			fi := fileInfo{FileNum: diskFileNum}
			if stat, err := d.opts.FS.Stat(filename); err == nil {
				fi.FileSize = uint64(stat.Size())
			}
			obsoleteManifests = append(obsoleteManifests, fi)
		case fileTypeOptions:
			if diskFileNum >= d.optionsFileNum {
				continue
			}
			fi := fileInfo{FileNum: diskFileNum}
			if stat, err := d.opts.FS.Stat(filename); err == nil {
				fi.FileSize = uint64(stat.Size())
			}
			obsoleteOptions = append(obsoleteOptions, fi)
		case fileTypeTable:
			// Objects are handled through the objstorage provider below.
		default:
			// Don't delete files we don't know about.
		}
	}

	objects := d.objProvider.List()
	for _, obj := range objects {
		switch obj.FileType {
		case fileTypeTable:
			if _, ok := liveFileNums[obj.DiskFileNum]; ok {
				continue
			}
			fileInfo := fileInfo{
				FileNum: obj.DiskFileNum,
			}
			if size, err := d.objProvider.Size(obj); err == nil {
				fileInfo.FileSize = uint64(size)
			}
			obsoleteTables = append(obsoleteTables, tableInfo{
				fileInfo: fileInfo,
				isLocal:  !obj.IsRemote(),
			})

		default:
			// Ignore object types we don't know about.
		}
	}

	d.mu.versions.obsoleteTables = mergeTableInfos(d.mu.versions.obsoleteTables, obsoleteTables)
	d.mu.versions.updateObsoleteTableMetricsLocked()
	d.mu.versions.obsoleteManifests = merge(d.mu.versions.obsoleteManifests, obsoleteManifests)
	d.mu.versions.obsoleteOptions = merge(d.mu.versions.obsoleteOptions, obsoleteOptions)
}

// disableFileDeletions disables file deletions and then waits for any
// in-progress deletion to finish. The caller is required to call
// enableFileDeletions in order to enable file deletions again. It is ok for
// multiple callers to disable file deletions simultaneously, though they must
// all invoke enableFileDeletions in order for file deletions to be re-enabled
// (there is an internal reference count on file deletion disablement).
//
// d.mu must be held when calling this method.
func (d *DB) disableFileDeletions() {
	d.mu.disableFileDeletions++
	d.mu.Unlock()
	defer d.mu.Lock()
	d.cleanupManager.Wait()
}

// enableFileDeletions enables previously disabled file deletions. A cleanup job
// is queued if necessary.
//
// d.mu must be held when calling this method.
func (d *DB) enableFileDeletions() {
	if d.mu.disableFileDeletions <= 0 {
		panic("pebble: file deletion disablement invariant violated")
	}
	d.mu.disableFileDeletions--
	if d.mu.disableFileDeletions > 0 {
		return
	}
	d.deleteObsoleteFiles(d.newJobIDLocked())
}

type fileInfo = base.FileInfo

// deleteObsoleteFiles enqueues a cleanup job to the cleanup manager, if necessary.
//
// d.mu must be held when calling this. The function will release and re-aquire the mutex.
//
// Does nothing if file deletions are disabled (see disableFileDeletions). A
// cleanup job will be scheduled when file deletions are re-enabled.
func (d *DB) deleteObsoleteFiles(jobID JobID) {
	if d.mu.disableFileDeletions > 0 {
		return
	}
	_, noRecycle := d.opts.Cleaner.(base.NeedsFileContents)

	// NB: d.mu.versions.minUnflushedLogNum is the log number of the earliest
	// log that has not had its contents flushed to an sstable.
	obsoleteLogs, err := d.mu.log.manager.Obsolete(wal.NumWAL(d.mu.versions.minUnflushedLogNum), noRecycle)
	if err != nil {
		panic(err)
	}

	obsoleteTables := append([]tableInfo(nil), d.mu.versions.obsoleteTables...)
	d.mu.versions.obsoleteTables = nil

	for _, tbl := range obsoleteTables {
		delete(d.mu.versions.zombieTables, tbl.FileNum)
	}

	// Sort the manifests cause we want to delete some contiguous prefix
	// of the older manifests.
	slices.SortFunc(d.mu.versions.obsoleteManifests, func(a, b fileInfo) int {
		return cmp.Compare(a.FileNum, b.FileNum)
	})

	var obsoleteManifests []fileInfo
	manifestsToDelete := len(d.mu.versions.obsoleteManifests) - d.opts.NumPrevManifest
	if manifestsToDelete > 0 {
		obsoleteManifests = d.mu.versions.obsoleteManifests[:manifestsToDelete]
		d.mu.versions.obsoleteManifests = d.mu.versions.obsoleteManifests[manifestsToDelete:]
		if len(d.mu.versions.obsoleteManifests) == 0 {
			d.mu.versions.obsoleteManifests = nil
		}
	}

	obsoleteOptions := d.mu.versions.obsoleteOptions
	d.mu.versions.obsoleteOptions = nil

	// Release d.mu while preparing the cleanup job and possibly waiting.
	// Note the unusual order: Unlock and then Lock.
	d.mu.Unlock()
	defer d.mu.Lock()

	filesToDelete := make([]obsoleteFile, 0, len(obsoleteLogs)+len(obsoleteTables)+len(obsoleteManifests)+len(obsoleteOptions))
	for _, f := range obsoleteLogs {
		filesToDelete = append(filesToDelete, obsoleteFile{fileType: fileTypeLog, logFile: f})
	}
	// We sort to make the order of deletions deterministic, which is nice for
	// tests.
	slices.SortFunc(obsoleteTables, func(a, b tableInfo) int {
		return cmp.Compare(a.FileNum, b.FileNum)
	})
	for _, f := range obsoleteTables {
		d.tableCache.evict(f.FileNum)
		filesToDelete = append(filesToDelete, obsoleteFile{
			fileType: fileTypeTable,
			nonLogFile: deletableFile{
				dir:      d.dirname,
				fileNum:  f.FileNum,
				fileSize: f.FileSize,
				isLocal:  f.isLocal,
			},
		})
	}
	files := [2]struct {
		fileType fileType
		obsolete []fileInfo
	}{
		{fileTypeManifest, obsoleteManifests},
		{fileTypeOptions, obsoleteOptions},
	}
	for _, f := range files {
		// We sort to make the order of deletions deterministic, which is nice for
		// tests.
		slices.SortFunc(f.obsolete, func(a, b fileInfo) int {
			return cmp.Compare(a.FileNum, b.FileNum)
		})
		for _, fi := range f.obsolete {
			dir := d.dirname
			filesToDelete = append(filesToDelete, obsoleteFile{
				fileType: f.fileType,
				nonLogFile: deletableFile{
					dir:      dir,
					fileNum:  fi.FileNum,
					fileSize: fi.FileSize,
					isLocal:  true,
				},
			})
		}
	}
	if len(filesToDelete) > 0 {
		d.cleanupManager.EnqueueJob(jobID, filesToDelete)
	}
	if d.opts.private.testingAlwaysWaitForCleanup {
		d.cleanupManager.Wait()
	}
}

func (d *DB) maybeScheduleObsoleteTableDeletion() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.maybeScheduleObsoleteTableDeletionLocked()
}

func (d *DB) maybeScheduleObsoleteTableDeletionLocked() {
	if len(d.mu.versions.obsoleteTables) > 0 {
		d.deleteObsoleteFiles(d.newJobIDLocked())
	}
}

func merge(a, b []fileInfo) []fileInfo {
	if len(b) == 0 {
		return a
	}

	a = append(a, b...)
	slices.SortFunc(a, func(a, b fileInfo) int {
		return cmp.Compare(a.FileNum, b.FileNum)
	})
	return slices.CompactFunc(a, func(a, b fileInfo) bool {
		return a.FileNum == b.FileNum
	})
}

func mergeTableInfos(a, b []tableInfo) []tableInfo {
	if len(b) == 0 {
		return a
	}

	a = append(a, b...)
	slices.SortFunc(a, func(a, b tableInfo) int {
		return cmp.Compare(a.FileNum, b.FileNum)
	})
	return slices.CompactFunc(a, func(a, b tableInfo) bool {
		return a.FileNum == b.FileNum
	})
}
