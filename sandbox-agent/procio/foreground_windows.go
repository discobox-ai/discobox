package procio

import "os"

// ForegroundGroup reports no group: a Windows console has no process groups
// to read.
func ForegroundGroup(*os.File) int { return 0 }
