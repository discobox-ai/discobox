package boot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/sandboxtree"
	"github.com/discobox-ai/discobox/tarsums"
)

// exportMountPaths are where the export mode reads each subtree it may be asked
// for: the primary volumes, not the wired targets, so what it reads is what the
// pool stores. Origins are not here: they are the pool's own and it adds them
// itself (ADR 0129 §1).
var exportMountPaths = map[string]string{
	sandboxtree.Data:    dataMountPath,
	sandboxtree.Sources: sourcesMountPath,
}

// Export is the export mode (ADR 0129 §1): it writes the named subtrees of a
// stopped sandbox's durable tree to out as a tree archive, leaving out every
// data path the image declared excludeFromExport, and returns the process exit
// code.
//
// It runs as PID 1 of a one-shot container started from the sandbox's own
// image and never starts anything else, so nothing the user runs is running
// while the tree is read. It resolves the sandbox user and the declared volumes
// exactly as boot does -- only the sandbox can say where a %HOME% path lives
// (ADR 0033) -- but wires, chowns and writes nothing: the pool mounts the trees
// read-only.
//
// Only the archive goes to out. Everything said about it goes to the logger,
// which the pool agent reads as the reason when the mode fails.
func Export(ctx context.Context, logger *slog.Logger, subtrees []string, out io.Writer) int {
	if logger == nil {
		logger = slog.Default()
	}
	if err := export(ctx, subtrees, out); err != nil {
		logger.Error("export sandbox tree", "error", err)
		return 1
	}
	return 0
}

func export(ctx context.Context, subtrees []string, out io.Writer) error {
	for _, subtree := range subtrees {
		if _, ok := exportMountPaths[subtree]; !ok {
			return fmt.Errorf("unknown subtree %q; the export mode reads %s and %s", subtree, sandboxtree.Data, sandboxtree.Sources)
		}
	}
	skip, err := exportExclusions()
	if err != nil {
		return err
	}
	archive := tarsums.NewWriter(out)
	writer := sandboxtree.NewWriter(archive)
	for _, subtree := range subtrees {
		if err := writer.AddDir(ctx, exportMountPaths[subtree], subtree, skip); err != nil {
			return fmt.Errorf("write %s: %w", subtree, err)
		}
	}
	// SHA256SUMS is written here and nowhere else, so every early return above
	// leaves an archive the pool agent refuses.
	return archive.Close()
}

// exportExclusions is the set of archive names the image declared stay behind,
// as a skip function for sandboxtree.Writer.
//
// A sandbox whose create never wrote sandbox.json has no declared volumes, so
// nothing is excluded and its tree travels whole.
func exportExclusions() (func(string) bool, error) {
	effective, err := loadEffectiveConfig()
	if err != nil {
		return nil, fmt.Errorf("load sandbox config: %w", err)
	}
	if len(effective.Volumes) == 0 {
		return nil, nil
	}
	// A booter because an account the manifest named without ids has to be
	// created before it can be resolved (resolveIdentity). That happens in this
	// one-shot container as it did in the sandbox's, from the same image, so
	// useradd gives it the same uid. Nothing else here runs a command.
	id, err := newBooter().resolveIdentity()
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox identity: %w", err)
	}
	volumes, err := loadResolvedVolumes(id, effective.Volumes)
	if err != nil {
		return nil, fmt.Errorf("resolve volumes: %w", err)
	}
	excluded := exportExcludedNames(volumes, id.uid)
	if len(excluded) == 0 {
		return nil, nil
	}
	return func(name string) bool {
		_, ok := excluded[name]
		return ok
	}, nil
}

// exportExcludedNames maps each data volume declared excludeFromExport to the
// archive name of its backing directory: volumeDir, which is also where an
// overlay's upper and work directories live, relocated from the data mount to
// the data subtree. A declared path nested beneath one is covered by the skip
// of its parent, since the walk never descends into a skipped directory.
func exportExcludedNames(volumes []harness.ResolvedVolume, uid int) map[string]struct{} {
	excluded := map[string]struct{}{}
	for _, v := range volumes {
		if !v.ExcludeFromExport || v.Kind != harness.VolumeData {
			continue
		}
		relative := strings.TrimPrefix(volumeDir(v, uid), dataMountPath)
		name := path.Join(sandboxtree.Data, relative)
		if name == sandboxtree.Data {
			// Only a declaration of "/" names the data root itself, and
			// honoring it would export no home at all.
			continue
		}
		excluded[name] = struct{}{}
	}
	return excluded
}
