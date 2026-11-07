//go:build windows

package endpoint

func platformDefaultEndpoint() string {
	return `npipe:////./pipe/discobox`
}
