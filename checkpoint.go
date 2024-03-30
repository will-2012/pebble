// Copyright 2019 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"io"
	"os"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/oserror"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/record"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/pebble/vfs/atomicfs"
	"github.com/cockroachdb/pebble/wal"
)

// checkpointOptions hold the optional parameters to construct checkpoint
// snapshots.
type checkpointOptions struct {
	// flushWAL set to true will force a flush and sync of the WAL prior to
	// checkpointing.
	flushWAL bool

	// If set, any SSTs that don't overlap with these spans are excluded from a checkpoint.
	restrictToSpans []CheckpointSpan

	// concurrentLinkOrCopy set concurrent worker to copy or link SST to
	// speedup checkpoint.
	concurrentLinkOrCopy uint64
}

// CheckpointOption set optional parameters used by `DB.Checkpoint`.
type CheckpointOption func(*checkpointOptions)

// WithFlushedWAL enables flushing and syncing the WAL prior to constructing a
// checkpoint. This guarantees that any writes committed before calling
// DB.Checkpoint will be part of that checkpoint.
//
// Note that this setting can only be useful in cases when some writes are
// performed with Sync = false. Otherwise, the guarantee will already be met.
//
// Passing this option is functionally equivalent to calling
// DB.LogData(nil, Sync) right before DB.Checkpoint.
func WithFlushedWAL() CheckpointOption {
	return func(opt *checkpointOptions) {
		opt.flushWAL = true
	}
}

// ConcurrentLinkOrCopy controls concurrent copy or copy of sst files to improve checkpoint speed.
func ConcurrentLinkOrCopy(concurrent uint64) CheckpointOption {
	return func(opt *checkpointOptions) {
		opt.concurrentLinkOrCopy = concurrent
	}
}

// WithRestrictToSpans specifies spans of interest for the checkpoint. Any SSTs
// that don't overlap with any of these spans are excluded from the checkpoint.
//
// Note that the checkpoint can still surface keys outside of these spans (from
// the WAL and from SSTs that partially overlap with these spans). Moreover,
// these surface keys aren't necessarily "valid" in that they could have been
// modified but the SST containing the modification is excluded.
func WithRestrictToSpans(spans []CheckpointSpan) CheckpointOption {
	return func(opt *checkpointOptions) {
		opt.restrictToSpans = spans
	}
}

// CheckpointSpan is a key range [Start, End) (inclusive on Start, exclusive on
// End) of interest for a checkpoint.
type CheckpointSpan struct {
	Start []byte
	End   []byte
}

// excludeFromCheckpoint returns true if an SST file should be excluded from the
// checkpoint because it does not overlap with the spans of interest
// (opt.restrictToSpans).
func excludeFromCheckpoint(f *fileMetadata, opt *checkpointOptions, cmp Compare) bool {
	if len(opt.restrictToSpans) == 0 {
		// Option not set; don't exclude anything.
		return false
	}
	for _, s := range opt.restrictToSpans {
		spanBounds := base.UserKeyBoundsEndExclusive(s.Start, s.End)
		if f.Overlaps(cmp, &spanBounds) {
			return false
		}
	}
	// None of the restrictToSpans overlapped; we can exclude this file.
	return true
}

// mkdirAllAndSyncParents creates destDir and any of its missing parents.
// Those missing parents, as well as the closest existing ancestor, are synced.
// Returns a handle to the directory created at destDir.
func mkdirAllAndSyncParents(fs vfs.FS, destDir string) (vfs.File, error) {
	// Collect paths for all directories between destDir (excluded) and its
	// closest existing ancestor (included).
	var parentPaths []string
	foundExistingAncestor := false
	for parentPath := fs.PathDir(destDir); parentPath != "."; parentPath = fs.PathDir(parentPath) {
		parentPaths = append(parentPaths, parentPath)
		_, err := fs.Stat(parentPath)
		if err == nil {
			// Exit loop at the closest existing ancestor.
			foundExistingAncestor = true
			break
		}
		if !oserror.IsNotExist(err) {
			return nil, err
		}
	}
	// Handle empty filesystem edge case.
	if !foundExistingAncestor {
		parentPaths = append(parentPaths, "")
	}
	// Create destDir and any of its missing parents.
	if err := fs.MkdirAll(destDir, 0755); err != nil {
		return nil, err
	}
	// Sync all the parent directories up to the closest existing ancestor,
	// included.
	for _, parentPath := range parentPaths {
		parentDir, err := fs.OpenDir(parentPath)
		if err != nil {
			return nil, err
		}
		err = parentDir.Sync()
		if err != nil {
			_ = parentDir.Close()
			return nil, err
		}
		err = parentDir.Close()
		if err != nil {
			return nil, err
		}
	}
	return fs.OpenDir(destDir)
}

// Checkpoint constructs a snapshot of the DB instance in the specified
// directory. The WAL, MANIFEST, OPTIONS, and sstables will be copied into the
// snapshot. Hard links will be used when possible. Beware of the significant
// space overhead for a checkpoint if hard links are disabled. Also beware that
// even if hard links are used, the space overhead for the checkpoint will
// increase over time as the DB performs compactions.
//
// Note that shared files in a checkpoint could get deleted if the DB is
// restarted after a checkpoint operation, as the reference for the checkpoint
// is only maintained in memory. This is okay as long as users of Checkpoint
// crash shortly afterwards with a "poison file" preventing further restarts.
func (d *DB) Checkpoint(
	destDir string, opts ...CheckpointOption,
) (
	ckErr error, /* used in deferred cleanup */
) {
	opt := &checkpointOptions{}
	for _, fn := range opts {
		fn(opt)
	}
	if opt.concurrentLinkOrCopy == 0 {
		opt.concurrentLinkOrCopy = 1 // sanitize concurrent option.
	}

	if _, err := d.opts.FS.Stat(destDir); !oserror.IsNotExist(err) {
		if err == nil {
			return &os.PathError{
				Op:   "checkpoint",
				Path: destDir,
				Err:  oserror.ErrExist,
			}
		}
		return err
	}

	if opt.flushWAL && !d.opts.DisableWAL {
		// Write an empty log-data record to flush and sync the WAL.
		if err := d.LogData(nil /* data */, Sync); err != nil {
			return err
		}
	}

	// Disable file deletions.
	d.mu.Lock()
	d.disableFileDeletions()
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.enableFileDeletions()
	}()

	// TODO(peter): RocksDB provides the option to roll the manifest if the
	// MANIFEST size is too large. Should we do this too?

	// Lock the manifest before getting the current version. We need the
	// length of the manifest that we read to match the current version that
	// we read, otherwise we might copy a versionEdit not reflected in the
	// sstables we copy/link.
	d.mu.versions.logLock()
	// Get the unflushed log files, the current version, and the current manifest
	// file number.
	memQueue := d.mu.mem.queue
	current := d.mu.versions.currentVersion()
	formatVers := d.FormatMajorVersion()
	manifestFileNum := d.mu.versions.manifestFileNum
	manifestSize := d.mu.versions.manifest.Size()
	optionsFileNum := d.optionsFileNum

	virtualBackingFiles := make(map[base.DiskFileNum]struct{})
	d.mu.versions.virtualBackings.ForEach(func(backing *fileBacking) {
		virtualBackingFiles[backing.DiskFileNum] = struct{}{}
	})

	queuedLogNums := make([]wal.NumWAL, 0, len(memQueue))
	for i := range memQueue {
		if logNum := memQueue[i].logNum; logNum != 0 {
			queuedLogNums = append(queuedLogNums, wal.NumWAL(logNum))
		}
	}
	// Release the manifest and DB.mu so we don't block other operations on
	// the database.
	d.mu.versions.logUnlock()
	d.mu.Unlock()

	allLogicalLogs, err := d.mu.log.manager.List()
	if err != nil {
		return err
	}

	// Wrap the normal filesystem with one which wraps newly created files with
	// vfs.NewSyncingFile.
	fs := vfs.NewSyncingFS(d.opts.FS, vfs.SyncingFileOptions{
		NoSyncOnClose: d.opts.NoSyncOnClose,
		BytesPerSync:  d.opts.BytesPerSync,
	})

	// Create the dir and its parents (if necessary), and sync them.
	var dir vfs.File
	defer func() {
		if dir != nil {
			_ = dir.Close()
		}
		if ckErr != nil {
			// Attempt to cleanup on error.
			_ = fs.RemoveAll(destDir)
		}
	}()
	dir, ckErr = mkdirAllAndSyncParents(fs, destDir)
	if ckErr != nil {
		return ckErr
	}

	{
		// Link or copy the OPTIONS.
		srcPath := base.MakeFilepath(fs, d.dirname, fileTypeOptions, optionsFileNum)
		destPath := fs.PathJoin(destDir, fs.PathBase(srcPath))
		ckErr = vfs.LinkOrCopy(fs, srcPath, destPath)
		if ckErr != nil {
			return ckErr
		}
	}

	{
		// Set the format major version in the destination directory.
		var versionMarker *atomicfs.Marker
		versionMarker, _, ckErr = atomicfs.LocateMarker(fs, destDir, formatVersionMarkerName)
		if ckErr != nil {
			return ckErr
		}

		// We use the marker to encode the active format version in the
		// marker filename. Unlike other uses of the atomic marker,
		// there is no file with the filename `formatVers.String()` on
		// the filesystem.
		ckErr = versionMarker.Move(formatVers.String())
		if ckErr != nil {
			return ckErr
		}
		ckErr = versionMarker.Close()
		if ckErr != nil {
			return ckErr
		}
	}

	// concurrent speedup SST copy or link.
	var (
		concurrentCh        = make(chan struct{}, opt.concurrentLinkOrCopy)
		hasFailedLinkOrCopy atomic.Bool
	)
	hasFailedLinkOrCopy.Store(false)

	var excludedFiles map[deletedFileEntry]*fileMetadata
	var remoteFiles []base.DiskFileNum
	// Set of FileBacking.DiskFileNum which will be required by virtual sstables
	// in the checkpoint.
	requiredVirtualBackingFiles := make(map[base.DiskFileNum]struct{})
	// Link or copy the sstables.
	for l := range current.Levels {
		iter := current.Levels[l].Iter()
		for f := iter.First(); f != nil; f = iter.Next() {
			if excludeFromCheckpoint(f, opt, d.cmp) {
				if excludedFiles == nil {
					excludedFiles = make(map[deletedFileEntry]*fileMetadata)
				}
				excludedFiles[deletedFileEntry{
					Level:   l,
					FileNum: f.FileNum,
				}] = f
				continue
			}

			fileBacking := f.FileBacking
			if f.Virtual {
				if _, ok := requiredVirtualBackingFiles[fileBacking.DiskFileNum]; ok {
					continue
				}
				requiredVirtualBackingFiles[fileBacking.DiskFileNum] = struct{}{}
			}
			meta, err := d.objProvider.Lookup(fileTypeTable, fileBacking.DiskFileNum)
			if err != nil {
				ckErr = err
				return ckErr
			}
			if meta.IsRemote() {
				// We don't copy remote files. This is desirable as checkpointing is
				// supposed to be a fast operation, and references to remote files can
				// always be resolved by any checkpoint readers by reading the object
				// catalog. We don't add this file to excludedFiles either, as that'd
				// cause it to be deleted in the second manifest entry which is also
				// inaccurate.
				remoteFiles = append(remoteFiles, meta.DiskFileNum)
				continue
			}

			concurrentCh <- struct{}{}
			go func() {
				srcPath := base.MakeFilepath(fs, d.dirname, fileTypeTable, fileBacking.DiskFileNum)
				destPath := fs.PathJoin(destDir, fs.PathBase(srcPath))
				innerCkErr := vfs.LinkOrCopy(fs, srcPath, destPath)
				if innerCkErr != nil && hasFailedLinkOrCopy.CompareAndSwap(false, true) {
					ckErr = innerCkErr
				}
				<-concurrentCh
			}()
			if hasFailedLinkOrCopy.Load() {
				break
			}
		}
		if hasFailedLinkOrCopy.Load() {
			break
		}
	}

	// wait concurrent finish.
	for i := uint64(0); i < opt.concurrentLinkOrCopy; i++ {
		concurrentCh <- struct{}{}
	}
	if hasFailedLinkOrCopy.Load() {
		return ckErr
	}

	var removeBackingTables []base.DiskFileNum
	for diskFileNum := range virtualBackingFiles {
		if _, ok := requiredVirtualBackingFiles[diskFileNum]; !ok {
			// The backing sstable associated with fileNum is no longer
			// required.
			removeBackingTables = append(removeBackingTables, diskFileNum)
		}
	}

	ckErr = d.writeCheckpointManifest(
		fs, formatVers, destDir, dir, manifestFileNum, manifestSize,
		excludedFiles, removeBackingTables,
	)
	if ckErr != nil {
		return ckErr
	}
	if len(remoteFiles) > 0 {
		ckErr = d.objProvider.CheckpointState(fs, destDir, fileTypeTable, remoteFiles)
		if ckErr != nil {
			return ckErr
		}
	}

	// Copy the WAL files. We copy rather than link because WAL file recycling
	// will cause the WAL files to be reused which would invalidate the
	// checkpoint.
	for _, logNum := range queuedLogNums {
		log, ok := allLogicalLogs.Get(logNum)
		if !ok {
			return errors.Newf("log %s not found", logNum)
		}
		for i := 0; i < log.NumSegments(); i++ {
			srcFS, srcPath := log.SegmentLocation(i)
			destPath := fs.PathJoin(destDir, srcFS.PathBase(srcPath))
			ckErr = vfs.CopyAcrossFS(srcFS, srcPath, fs, destPath)
			if ckErr != nil {
				return ckErr
			}
		}
	}

	// Sync and close the checkpoint directory.
	ckErr = dir.Sync()
	if ckErr != nil {
		return ckErr
	}
	ckErr = dir.Close()
	dir = nil
	return ckErr
}

func (d *DB) writeCheckpointManifest(
	fs vfs.FS,
	formatVers FormatMajorVersion,
	destDirPath string,
	destDir vfs.File,
	manifestFileNum base.DiskFileNum,
	manifestSize int64,
	excludedFiles map[deletedFileEntry]*fileMetadata,
	removeBackingTables []base.DiskFileNum,
) error {
	// Copy the MANIFEST, and create a pointer to it. We copy rather
	// than link because additional version edits added to the
	// MANIFEST after we took our snapshot of the sstables will
	// reference sstables that aren't in our checkpoint. For a
	// similar reason, we need to limit how much of the MANIFEST we
	// copy.
	// If some files are excluded from the checkpoint, also append a block that
	// records those files as deleted.
	if err := func() error {
		srcPath := base.MakeFilepath(fs, d.dirname, fileTypeManifest, manifestFileNum)
		destPath := fs.PathJoin(destDirPath, fs.PathBase(srcPath))
		src, err := fs.Open(srcPath, vfs.SequentialReadsOption)
		if err != nil {
			return err
		}
		defer src.Close()

		dst, err := fs.Create(destPath)
		if err != nil {
			return err
		}
		defer dst.Close()

		// Copy all existing records. We need to copy at the record level in case we
		// need to append another record with the excluded files (we cannot simply
		// append a record after a raw data copy; see
		// https://github.com/cockroachdb/cockroach/issues/100935).
		r := record.NewReader(&io.LimitedReader{R: src, N: manifestSize}, manifestFileNum)
		w := record.NewWriter(dst)
		for {
			rr, err := r.Next()
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}

			rw, err := w.Next()
			if err != nil {
				return err
			}
			if _, err := io.Copy(rw, rr); err != nil {
				return err
			}
		}

		if len(excludedFiles) > 0 {
			// Write out an additional VersionEdit that deletes the excluded SST files.
			ve := versionEdit{
				DeletedFiles:         excludedFiles,
				RemovedBackingTables: removeBackingTables,
			}

			rw, err := w.Next()
			if err != nil {
				return err
			}
			if err := ve.Encode(rw); err != nil {
				return err
			}
		}
		if err := w.Close(); err != nil {
			return err
		}
		return dst.Sync()
	}(); err != nil {
		return err
	}

	var manifestMarker *atomicfs.Marker
	manifestMarker, _, err := atomicfs.LocateMarker(fs, destDirPath, manifestMarkerName)
	if err != nil {
		return err
	}
	if err := manifestMarker.Move(base.MakeFilename(fileTypeManifest, manifestFileNum)); err != nil {
		return err
	}
	return manifestMarker.Close()
}
