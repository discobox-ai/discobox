package poolagent

import (
	"context"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// FilesystemUsage is one filesystem as statfs describes it.
//
// It measures the whole backing filesystem, which on most hosts holds more than
// Discobox: UsedBytes is the filesystem's, not this pool's. What Discobox
// itself holds is the walked totals in PoolStorage (ADR 0071 resource accounting, consequences).
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

// SandboxStorage is one sandbox's footprint: everything on the pool's disk
// that is its own, which nothing else uses and which goes when it is purged
// (ADR 26-11-14-896 §1).
//
// There is deliberately no cache figure here. Cache is one pool-shared tree
// keyed by the target path a harness declared and, for every path that did not
// declare itself shared, by the sandbox user's uid above it -- never by which
// sandbox wrote it (ADR 0007, ADR 0050, ADR 0094 cache partition), so a per-sandbox cache size
// has no on-disk answer.
// Repeating the shared total on every sandbox would make this column stop
// summing to anything real, so cache is reported once, at the pool (ADR 0071 resource accounting §5).
type SandboxStorage struct {
	SandboxID    string `json:"sandboxId"`
	DataBytes    int64  `json:"dataBytes"`
	ConfigBytes  int64  `json:"configBytes"`
	SourcesBytes int64  `json:"sourcesBytes"`
	SecretsBytes int64  `json:"secretsBytes"`
	OriginsBytes int64  `json:"originsBytes"`
	// LayerBytes is its container's writable layer, where its root
	// filesystem outside its volumes lives. Nil where the runtime keeps no
	// box on this pool's disk, or the sweep could not ask its daemon; zero
	// for a box with no container.
	LayerBytes *int64 `json:"layerBytes,omitempty"`
	// RecordingBytes is what the pool proxy recorded of its traffic: its
	// body and stream spools.
	RecordingBytes int64 `json:"recordingBytes"`
	// TotalBytes is every figure above, summed.
	TotalBytes int64 `json:"totalBytes"`
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
	// DataBytes is every sandbox's own figure (SandboxStorage.TotalBytes),
	// summed: its trees, its container's writable layer and its recordings.
	// It is the sandboxes' own storage and nothing shared — each tree holds
	// that sandbox's home, its sources and its nested container store, which
	// are its own copies rather than links into anything, so the sum is real
	// disk rather than one tree counted N times. On a shared daemon its
	// layers may be on another filesystem (DaemonShared).
	DataBytes int64 `json:"dataBytes"`
	// CacheBytes is the pool's shared cache tree, counted once for the pool
	// because that is exactly what it is.
	CacheBytes int64 `json:"cacheBytes"`
	// BuildBytes is buildkitd's store and the pool registry's blobs, which no
	// sandbox can reach and which no sandbox figure accounts for (ADR 0050).
	BuildBytes int64 `json:"buildBytes"`
	// ProxyCacheBytes is the pool proxy's response cache, shared by the
	// pool's sandboxes as the cache tree is.
	ProxyCacheBytes int64 `json:"proxyCacheBytes"`
	// ProxyAuditBytes is the proxy's audit database, with its write-ahead
	// log and shared-memory files.
	ProxyAuditBytes int64 `json:"proxyAuditBytes"`
	// ProxyRecordingBytes is the spools no sandbox in this sweep owns: a
	// purged sandbox's from before a purge took them, and a client's that is
	// no sandbox. Every other spool is on its sandbox's row.
	ProxyRecordingBytes int64 `json:"proxyRecordingBytes"`
	// ImageBytes is the daemon's images that are Discobox's, each layer
	// counted once on a dedicated daemon and estimated on a shared one
	// (DockerSandboxRuntime.DiskUsage), charged to the pool and never split across the sandboxes
	// that run them (ADR 26-11-14-896 §2). Nil where the runtime has no
	// daemon on this pool's disk, or its daemon did not answer this sweep:
	// absent, not zero.
	ImageBytes *int64 `json:"imageBytes,omitempty"`
	// OtherLayerBytes is the writable layers of this pool's containers that
	// are no sandbox's, such as a snapshot build's. Nil as ImageBytes is.
	OtherLayerBytes *int64 `json:"otherLayerBytes,omitempty"`
	// OtherFilesystemBytes is how much of the walked figures above is on a
	// filesystem other than the one Filesystem measures: a VM pool keeps its
	// cache and build trees on a disk of their own. A reader comparing what
	// is attributed with the filesystem's used figure leaves it out.
	OtherFilesystemBytes int64 `json:"otherFilesystemBytes,omitempty"`
	// DaemonShared is whether the daemon those are measured on is shared with
	// anything but this pool, so its storage may be on another filesystem
	// and hold what is not Discobox's: a reader subtracts the images and
	// layers from the filesystem's used figure only when it is not (ADR
	// 26-11-14-896 §4).
	DaemonShared bool `json:"daemonShared,omitempty"`
	// Sandboxes is this sweep's per-sandbox result. It never leaves the agent
	// on the pool's own storage record — each entry is attached to its own
	// sandbox in the report, so the figure lives in one place.
	Sandboxes []SandboxStorage `json:"-"`
}

// poolFilesystem is the cheap half: one statfs of the tree everything lives
// under, taken on every report.
func poolFilesystem(root layout.Root) PoolStorage {
	storage := PoolStorage{Root: root.Dir()}
	if usage, ok := filesystemUsage(root.Dir()); ok {
		storage.Filesystem = usage
	}
	return storage
}

// walkPoolTrees is the expensive half: one pass over every inode this pool
// owns, so each tree can be attributed to what put it there, joined with what
// the runtime's own store holds for it (disk; nil when the runtime could not
// say this sweep, which leaves those figures absent).
//
// sandboxIDs is what the runtime says this pool hosts. A sandbox whose tree has
// already been reaped contributes zeroes rather than being dropped, so a
// consumer sees the sandbox it asked about instead of a silent gap.
//
// A canceled walk returns false and no partial result. A sweep that stopped
// half way through would report every unvisited tree as empty, which is a
// wrong answer rather than a missing one.
func walkPoolTrees(ctx context.Context, root layout.Root, projectID, poolID string, sandboxIDs []string, disk *sandboxruntime.DiskUsage) (*PoolStorageWalk, bool) {
	started := time.Now()
	walk := &PoolStorageWalk{
		CacheBytes:      treeBytes(ctx, root.PoolCache(projectID, poolID)),
		BuildBytes:      treeBytes(ctx, root.PoolBuild(projectID, poolID)),
		ProxyCacheBytes: treeBytes(ctx, root.ProxyCache(projectID, poolID)),
		ProxyAuditBytes: auditDatabaseBytes(root.ProxyAuditDB(projectID, poolID)),
		Sandboxes:       make([]SandboxStorage, 0, len(sandboxIDs)),
	}
	// The proxy spools each client's recordings in a directory named by its
	// ID (proxy/internal/audit: bodies/<client>, streams/<client>), so one
	// pass over each tree attributes them all.
	recordings, loose := clientTreeBytes(ctx, filepath.Join(root.ProxyBodies(projectID, poolID), "bodies"))
	streams, looseStreams := clientTreeBytes(ctx, filepath.Join(root.ProxyStreams(projectID, poolID), "streams"))
	for client, bytes := range streams {
		recordings[client] += bytes
	}
	walk.ProxyRecordingBytes = loose + looseStreams
	// A tree on another disk than the root's is not in the filesystem
	// figure, so what it holds is said to be elsewhere.
	rootDevice, rootKnown := statDevice(root.Dir())
	elsewhere := func(path string, bytes int64) {
		if device, ok := statDevice(path); ok && rootKnown && device != rootDevice {
			walk.OtherFilesystemBytes += bytes
		}
	}
	elsewhere(root.PoolCache(projectID, poolID), walk.CacheBytes)
	elsewhere(root.PoolBuild(projectID, poolID), walk.BuildBytes)
	elsewhere(root.ProxyCache(projectID, poolID), walk.ProxyCacheBytes)
	elsewhere(filepath.Dir(root.ProxyAuditDB(projectID, poolID)), walk.ProxyAuditBytes)
	measured := disk != nil && disk.Measured
	if measured {
		images, other := disk.ImageBytes, disk.OtherLayerBytes
		walk.ImageBytes, walk.OtherLayerBytes = &images, &other
		walk.DaemonShared = disk.DaemonShared
	}
	for _, sandboxID := range sandboxIDs {
		entry := SandboxStorage{
			SandboxID:    sandboxID,
			DataBytes:    treeBytes(ctx, root.SandboxData(projectID, poolID, sandboxID)),
			ConfigBytes:  treeBytes(ctx, root.SandboxConfig(projectID, poolID, sandboxID)),
			SourcesBytes: treeBytes(ctx, root.SandboxSources(projectID, poolID, sandboxID)),
			SecretsBytes: treeBytes(ctx, root.SandboxSecrets(projectID, poolID, sandboxID)),
			OriginsBytes: treeBytes(ctx, root.SandboxOrigins(projectID, poolID, sandboxID)),
			// Taken out of the map, so what is left in it below is the
			// spools no sandbox of this sweep owns.
			RecordingBytes: recordings[sandboxID],
		}
		delete(recordings, sandboxID)
		entry.TotalBytes = entry.DataBytes + entry.ConfigBytes + entry.SourcesBytes +
			entry.SecretsBytes + entry.OriginsBytes + entry.RecordingBytes
		if measured {
			layer := disk.Layers[sandboxID]
			entry.LayerBytes = &layer
			entry.TotalBytes += layer
		}
		walk.DataBytes += entry.TotalBytes
		walk.Sandboxes = append(walk.Sandboxes, entry)
	}
	for _, bytes := range recordings {
		walk.ProxyRecordingBytes += bytes
	}
	// What is left in disk.Layers is containers of the pool's that no
	// sandbox of this sweep owns: one created since the sandboxes were
	// listed, or an orphan the reaper has not yet taken. They are not a
	// row's, so they are the pool's other layers rather than nobody's.
	var trees, recorded int64
	rows := make(map[string]struct{}, len(walk.Sandboxes))
	for _, entry := range walk.Sandboxes {
		trees += entry.DataBytes + entry.ConfigBytes + entry.SourcesBytes + entry.SecretsBytes + entry.OriginsBytes
		recorded += entry.RecordingBytes
		rows[entry.SandboxID] = struct{}{}
	}
	if measured {
		for sandboxID, layer := range disk.Layers {
			if _, ok := rows[sandboxID]; !ok {
				*walk.OtherLayerBytes += layer
			}
		}
	}
	elsewhere(root.PoolSandboxes(projectID, poolID), trees)
	elsewhere(root.ProxyBodies(projectID, poolID), recorded+walk.ProxyRecordingBytes)
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

// clientTreeBytes is treeBytes of each directory under dir, by its name, and
// of whatever lies loose in dir itself. A missing dir is no clients.
func clientTreeBytes(ctx context.Context, dir string) (map[string]int64, int64) {
	byClient := map[string]int64{}
	var loose int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return byClient, 0
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			byClient[entry.Name()] += treeBytes(ctx, path)
			continue
		}
		if info, err := entry.Info(); err == nil {
			loose += allocatedBytes(info)
		}
	}
	return byClient, loose
}

// auditDatabaseBytes is the proxy's SQLite audit database, which in WAL mode
// is three files: the database, its write-ahead log and its shared memory.
func auditDatabaseBytes(path string) int64 {
	var total int64
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(file); err == nil {
			total += allocatedBytes(info)
		}
	}
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
