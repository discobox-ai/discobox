package shimruntime

import "os"

// foregroundGroup reports no group: a Windows console has no process groups
// to read, so records are dropped only when the exec ends.
func foregroundGroup(*os.File) int { return 0 }
