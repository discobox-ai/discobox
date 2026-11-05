package endpoint

import "os"

type RuntimeDirectoryChange struct {
	Path string
	Mode os.FileMode
}
type RuntimeDirectoryRepair struct{}

func InspectRuntimeDirectories(string) (*RuntimeDirectoryRepair, error) { return nil, nil }
func (*RuntimeDirectoryRepair) Changes() []RuntimeDirectoryChange       { return nil }
func (*RuntimeDirectoryRepair) Close()                                  {}
func (*RuntimeDirectoryRepair) Repair() error                           { return nil }
