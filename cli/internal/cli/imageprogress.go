package cli

import (
	"fmt"
	"strings"
)

func bytesSuffix(current, total int64, layersDone, layers int) string {
	var tail string
	switch {
	case total > 0:
		tail = fmt.Sprintf(" — %s of %s", humanBytes(current), humanBytes(total))
	case current > 0:
		tail = fmt.Sprintf(" — %s", humanBytes(current))
	}
	if layers > 0 {
		tail += fmt.Sprintf(", %d/%d layers", layersDone, layers)
	}
	return tail
}

// shortImage is the part of a reference that identifies it. A status line has
// one line, and the registry and namespace are the same for every image here.
func shortImage(image string) string {
	if slash := strings.LastIndex(image, "/"); slash >= 0 {
		return image[slash+1:]
	}
	return image
}

// humanBytes renders a byte count the way a person reads one. Neither count is
// progress toward a fixed target — both grow while the manifest is walked — so
// this is a pair of counts and never a percentage, which would go backwards.
func humanBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value, exp := float64(bytes)/unit, 0
	for value >= unit && exp < 3 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGT"[exp])
}
