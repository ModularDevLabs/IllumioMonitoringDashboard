//go:build !windows

package extractor

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
