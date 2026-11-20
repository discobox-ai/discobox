// Package provisionprogress renders provisioning reports for CLI and server waits.
package provisionprogress

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// ProvisionProgressFresh bounds how old a recorded phase may be and still be
// reported as what is happening now.
//
// The record is the last report and is never cleared, so a sandbox that
// finished provisioning yesterday still carries the phase it finished in. Work
// that is genuinely underway reports continuously — the pull reports twice a
// second, the pool restates materializing a source on every read of the
// sandbox's clone and waiting for the agent every ten seconds of a slow boot,
// and every other phase is published as it is entered — so a
// phase that has not been restated in this long is describing the past.
const ProvisionProgressFresh = 30 * time.Second

// ImagePull renders the progress of an image pull with both of its ratios.
//
// Neither ratio is progress toward a fixed target — both totals grow while the
// manifest is walked — so this reports them as the pair of counts they are and
// never as a percentage, which would visibly go backwards.
//
// A layer being fetched again after its transfer ended is named on the line
// for as long as it is.
//
// An image loaded from the image cache is read into the daemon whole before
// any of it is extracted, so once extraction is reported the line counts that
// instead: the bytes read are already all of them, and the minutes left are in
// the layers.
func ImagePull(pull apimodel.SandboxPullProgress, operation string) string {
	line := operation + " " + DisplaySafe(imageLabel(pull.Image))
	current, total, layersComplete := pull.Current.Or(0), pull.Total.Or(0), pull.LayersComplete.Or(0)
	var extracting string
	if pull.Extracted.IsSet() || pull.LayersExtracted.IsSet() {
		extracting = "extracting "
		current, layersComplete = pull.Extracted.Or(0), pull.LayersExtracted.Or(0)
	}
	switch {
	case total > 0:
		line += fmt.Sprintf(" — %s%s of %s", extracting, Bytes(current), Bytes(total))
	case current > 0:
		line += fmt.Sprintf(" — %s%s", extracting, Bytes(current))
	}
	if layers := pull.Layers.Or(0); layers > 0 {
		line += fmt.Sprintf(", %d/%d layers", layersComplete, layers)
	}
	if retry, ok := pull.Retry.Get(); ok {
		// Said for as long as the layer is being fetched again, in the words
		// the server's image download uses, so a count that pauses or steps
		// back reads as the retry it is.
		line += fmt.Sprintf(", retrying layer %d", retry.Layer)
		if layers := pull.Layers.Or(0); layers > 0 {
			line += fmt.Sprintf(" of %d", layers)
		}
		if kept := retry.Kept.Or(0); kept > 0 {
			line += " after its transfer ended at " + Bytes(kept)
		}
	}
	return line
}

// DisplaySafe escapes every rune a terminal would act on rather than show,
// as its Go escape. A source's name can come from a URL that repository
// content declared (ADR 0056), and printed raw an escape sequence in it is an
// instruction to the reader's terminal (cli/REVIEW.md).
func DisplaySafe(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
			continue
		}
		quoted := strconv.QuoteRune(r)
		b.WriteString(quoted[1 : len(quoted)-1])
	}
	return b.String()
}

// imageLabel shortens an image reference to the part that identifies it. A
// status line has one line, and the registry host and namespace are the same
// for every image a given deployment pulls.
//
// A digest is cut to its first bytes for the same reason. A pinned reference is
// the intended form for the guest image, and its full digest is 71 characters
// of a line that also has to carry a byte count — long enough to push the
// numbers off the end of a terminal. The short form still tells two builds
// apart, which is all a status line needs it for; the whole digest is in the
// server's log.
func imageLabel(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return "the discobox image"
	}
	if slash := strings.LastIndex(image, "/"); slash >= 0 && slash+1 < len(image) {
		image = image[slash+1:]
	}
	name, digest, ok := strings.Cut(image, "@")
	if !ok {
		return image
	}
	if _, hex, ok := strings.Cut(digest, ":"); ok {
		digest = hex
	}
	const shortDigest = 12
	if len(digest) > shortDigest {
		digest = digest[:shortDigest]
	}
	return name + "@" + digest
}

// Bytes renders a byte count the way a download reports one.
func Bytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value, exponent := float64(bytes)/unit, 0
	for value >= unit && exponent < 3 {
		value /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGT"[exponent])
}

// PoolStatus is one line saying what a pool host is being brought
// through, or "" when its driver has nothing current to report.
//
// A sandbox waiting for a pool
// says only that it is waiting; the pool is where the minutes are actually
// going — fetching a VM disk image, booting the machine, pulling the pool-agent
// image — and the driver doing that work records it as it happens.
func PoolStatus(pool *apimodel.Pool) string {
	if pool == nil {
		return ""
	}
	progress, ok := pool.ProvisionProgress.Get()
	if !ok {
		return ""
	}
	// The same freshness rule the sandbox's own phase gets, for the same
	// reason: the record is the last report and is never cleared, so a pool
	// that came up yesterday still carries the phase it finished in.
	if observed, ok := pool.ProvisionProgressAt.Get(); ok {
		if time.Since(observed) > ProvisionProgressFresh {
			return ""
		}
	}
	if pull, ok := progress.Pull.Get(); ok {
		switch progress.Phase {
		case apiclientgen.PoolProvisionPhaseFetchingVMImage, apiclientgen.PoolProvisionPhasePullingPoolImage:
			return ImagePull(pull, "pulling")
		case apiclientgen.PoolProvisionPhaseLoadingPoolImage:
			return ImagePull(pull, "loading")
		}
	}
	if progress.Phase == apiclientgen.PoolProvisionPhasePreloadingImages {
		line := "preloading images (one-time setup per image version)"
		if pull, ok := progress.Pull.Get(); ok {
			line += ": " + strings.TrimPrefix(ImagePull(pull, "loading"), "loading ")
		}
		return line
	}
	switch progress.Phase {
	case apiclientgen.PoolProvisionPhaseFetchingVMImage:
		return "fetching the VM image"
	case apiclientgen.PoolProvisionPhaseStartingVM:
		return "starting the VM"
	case apiclientgen.PoolProvisionPhaseWaitingForDocker:
		return "waiting for Docker in the VM"
	case apiclientgen.PoolProvisionPhaseSyncingDevelopmentImages:
		// Only a development server ever reports this, and on a VM backend it
		// is minutes: the images are built from the local checkout on the
		// pool's own Docker. "preparing" rather than "building" because the
		// same phase covers copying them from a developer's daemon.
		return "preparing the development images"
	case apiclientgen.PoolProvisionPhaseLoadingPoolImage:
		return "loading the pool image from cache"
	case apiclientgen.PoolProvisionPhasePullingPoolImage:
		return "pulling the pool's image"
	case apiclientgen.PoolProvisionPhaseStartingPoolAgent:
		return "starting the pool agent"
	case apiclientgen.PoolProvisionPhaseWaitingForPoolAgent:
		return "waiting for the pool agent to come up"
	}
	// A phase this build does not know is still worth reporting: the server is
	// newer than this CLI, and its own word for what it is doing beats a blank.
	if phase := strings.TrimSpace(string(progress.Phase)); phase != "" {
		return DisplaySafe(strings.ReplaceAll(phase, "_", " "))
	}
	return ""
}
