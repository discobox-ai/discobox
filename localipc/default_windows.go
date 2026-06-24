//go:build windows

package localipc

func DefaultEndpoint() string {
	return `npipe:////./pipe/discobox`
}
