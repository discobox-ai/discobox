package poolagent

import (
	"context"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/discobox-ai/discobox/layout"
)

// FilesystemUsage is one filesystem as statfs describes it.
//
// It measures the whole backing filesystem, which on most hosts holds more than
// Discobox: UsedBytes is the filesystem's, not this pool's. What Discobox
// itself holds is the walked totals in PoolStorage (ADR 0071 consequences).
type FilesystemUsage struct {
	TotalBytes int64 `json:"totalBytes"`
	UsedBytes  int64 `json:"usedBytes"`
	FreeBytes  int64 `json:"freeBytes"`
}

// filesystemUsageFromBlocks is the shared arithmetic behind every platform's
// statfs. Free is what an unprivileged writer can actually use (bavail), while
// used is computed against the reserved-inclusive bfree, so used + free is
// deliberately less than total on a filesystem with reserved blocks — that
// reserve is neither used nor available.
func filesystemUsageFromBlocks(blockSize, blocks, free, avail uint64) FilesystemUsage {
	return FilesystemUsage{
		TotalBytes: scaleBlocks(blocks, blockSize),
		UsedBytes:  scaleBlocks(blocks-min(free, blocks), blockSize),
		FreeBytes:  scaleBlocks(avail, blockSize),
	}
}

func scaleBlocks(blocks, blockSize uint64) int64 {
	if blockSize == 0 || blocks > uint64(math.MaxInt64)/blockSize {
		return math.MaxInt64
	}
	return int64(blocks * blockSize)
}

// SandboxStorage is one sandbox's durable footprint, by the tree that holds it.
//
// There is deliberately no cache figure here. Cache is one pool-shared tree
// keyed by the target path a harness declared, never by which sandbox wrote it
// (ADR 0007, ADR 0050), so a per-sandbox cache size has no on-disk answer.
// Repeating the shared total on every sandbox would make this column stop
// summing to anything real, so cache is reported once, at the pool (ADR 0071 §5).
type SandboxStorage struct {
	SandboxID    string `json:"sandboxId"`
	DataBytes    int64  `json:"dataBytes"`
	ConfigBytes  int64  `json:"configBytes"`
	SourcesBytes int64  `json:"sourcesBytes"`
	SecretsBytes int64  `json:"secretsBytes"`
	OriginsBytes int64  `json:"originsBytes"`
	TotalBytes   int64  `json:"totalBytes"`
}

// PoolStorage is what this pool holds on disk.
//
// It is deliberately two things at two different freshnesses. Filesystem is one
// statfs — O(1), taken on every report, and the number that answers "am I about
// to run out of disk". Walk is the per-tree attribution, which costs one pass
// over every inode the pool owns and therefore runs on its own adaptive
// schedule (see storageScanner). Reporting them as one figure would either make
// the cheap number as stale as the expensive one or the expensive one as
// frequent as the cheap one.
type PoolStorage struct {
	Root       string          `json:"root"`
	Filesystem FilesystemUsage `json:"filesystem"`
	// Walk is nil until the first sweep completes, which is not the same as a
	// pool holding nothing: a pool whose first sweep is still running has no
	// attribution yet, and zeroes would claim it is empty.
	Walk *PoolStorageWalk `json:"walk,omitempty"`
}

// PoolStorageWalk is one completed sweep of the pool's trees, carrying when it
// happened and when the next one is due.
//
// Those timestamps are what make a cached figure honest. The objection to
// caching a size is that it is right at an unknown moment; stamping it with the
// moment answers the objection rather than avoiding it, and NextScanAt tells a
// reader how stale it is allowed to get before they should distrust it.
type PoolStorageWalk struct {
	ObservedAt time.Time `json:"observedAt"`
	// DurationMillis is what this sweep cost. It is also the input to the next
	// interval: the scanner spends a fixed fraction of wall time walking, so an
	// expensive pool is walked rarely and a cheap one often, with no operator
	// having to guess a number per pool.
	DurationMillis  int64     `json:"durationMillis"`
	IntervalSeconds float64   `json:"intervalSeconds"`
	NextScanAt      time.Time `json:"nextScanAt"`
	// DataBytes is every sandbox's durable tree, summed. It is the sandboxes'
	// own storage and nothing shared — each tree holds that sandbox's home,
	// its sources and its nested container store, which are its own copies
	// rather than links into anything, so the sum is real disk rather than one
	// tree counted N times.
	DataBytes int64 `json:"dataBytes"`
	// CacheBytes is the pool's shared cache tree, counted once for the pool
	// because that is exactly what it is.
	CacheBytes int64 `json:"cacheBytes"`
	// BuildBytes is buildkitd's store and the pool registry's blobs, which no
	// sandbox can reach and which no sandbox figure accounts for (ADR 0050).
	BuildBytes int64 `json:"buildBytes"`
	// Sandboxes is this sweep's per-sandbox result. It never leaves the agent
	// on the pool's own storage record — each entry is attached to its own
	// sandbox in the report, so the figure lives in one place.
	Sandboxes []SandboxStorage `json:"-"`
}

// poolFilesystem is the cheap half: one statfs of the tree everything lives
// under, taken on every report.
func poolFilesystem() PoolStorage {
	storage := PoolStorage{Root: layout.ContainerRoot}
	if usage, ok := filesystemUsage(layout.ContainerRoot); ok {
		storage.Filesystem = usage
	}
	return storage
}

// walkPoolTrees is the expensive half: one pass over every inode this pool
// owns, so each tree can be attributed to what put it there.
//
// sandboxIDs is what the runtime says this pool hosts. A sandbox whose tree has
// already been reaped contributes zeroes rather than being dropped, so a
// consumer sees the sandbox it asked about instead of a silent gap.
//
// A canceled walk returns false and no partial result. A sweep that stopped
// half way through would report every unvisited tree as empty, which is a
// wrong answer rather than a missing one.
func walkPoolTrees(ctx context.Context, projectID, poolID string, sandboxIDs []string) (*PoolStorageWalk, bool) {
	started := time.Now()
	walk := &PoolStorageWalk{
		CacheBytes: treeBytes(ctx, layout.PoolCache(projectID, poolID)),
		BuildBytes: treeBytes(ctx, layout.PoolBuild(projectID, poolID)),
		Sandboxes:  make([]SandboxStorage, 0, len(sandboxIDs)),
	}
	for _, sandboxID := range sandboxIDs {
		entry := SandboxStorage{
			SandboxID:    sandboxID,
			DataBytes:    treeBytes(ctx, layout.SandboxData(projectID, poolID, sandboxID)),
			ConfigBytes:  treeBytes(ctx, layout.SandboxConfig(projectID, poolID, sandboxID)),
			SourcesBytes: treeBytes(ctx, layout.SandboxSources(projectID, poolID, sandboxID)),
			SecretsBytes: treeBytes(ctx, layout.SandboxSecrets(projectID, poolID, sandboxID)),
			OriginsBytes: treeBytes(ctx, layout.SandboxOrigins(projectID, poolID, sandboxID)),
		}
		entry.TotalBytes = entry.DataBytes + entry.ConfigBytes + entry.SourcesBytes +
			entry.SecretsBytes + entry.OriginsBytes
		walk.DataBytes += entry.TotalBytes
		walk.Sandboxes = append(walk.Sandboxes, entry)
	}
	if ctx.Err() != nil {
		return nil, false
	}
	walk.ObservedAt = time.Now().UTC()
	walk.DurationMillis = time.Since(started).Milliseconds()
	return walk, true
}

// treeBytes is the disk a tree occupies, counted as allocated blocks rather
// than as apparent size — the same thing `du` reports and the same thing statfs
// charges against the filesystem, so the walked totals and the filesystem's own
// used figure are measured in comparable units. Apparent size would overstate a
// sparse file and understate nothing.
//
// A hard-linked file is counted once per link seen, which is what makes this
// the sandbox's footprint rather than the filesystem's: a link into a shared
// store still charges the sandbox that holds it.
//
// Errors are absorbed. This walk races the sandboxes it measures — a source
// tree is checked out and a cache is written while it runs — so a file that
// disappears mid-walk is ordinary. A missing tree reports zero, which is the
// truth for a sandbox that has not been created yet or has been reaped.
func treeBytes(ctx context.Context, root string) int64 {
	var total int64
	var seen int
	_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		// Checked in batches rather than per entry: a sweep of a million-inode
		// cache would otherwise pay a context read per file to answer a
		// question whose answer changes once. 512 entries is well under a
		// millisecond of walking, so shutdown is still prompt.
		seen++
		if seen%512 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// Skip what cannot be read, including a directory that vanished
			// under the walk, and keep counting the rest.
			return nil //nolint:nilerr // A partial total beats no total at all.
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil //nolint:nilerr // The file went away mid-walk.
		}
		total += allocatedBytes(info)
		return nil
	})
	return total
}

// allocatedBytes is the blocks a file actually occupies where the platform
// reports them, and its apparent size where it does not.
func allocatedBytes(info os.FileInfo) int64 {
	if blocks, ok := statBlocks(info); ok {
		return blocks
	}
	return info.Size()
}
