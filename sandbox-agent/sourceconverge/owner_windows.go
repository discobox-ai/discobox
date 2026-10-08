package sourceconverge

import (
	"io/fs"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// fileOwner has no answer on Windows, where the agent runs git as itself.
func fileOwner(fs.FileInfo) (uid, gid int64, ok bool) { return 0, 0, false }

func chownTo(string, *execs.User) error { return nil }
