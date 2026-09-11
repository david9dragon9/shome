//go:build !unix

package platform

import "fmt"

func statfs(path string) (FSStat, error) {
	return FSStat{}, fmt.Errorf("filesystem statistics are not implemented on this platform")
}
