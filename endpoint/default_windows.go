//go:build windows

package endpoint

func DefaultEndpoint() string {
	return `npipe:////./pipe/discobox`
}
