// Package errbad is a throwaway package for the T-1 CI negative check. Do not merge.
package errbad

import "os"

// Cleanup deliberately ignores the error returned by os.Remove.
func Cleanup() {
	os.Remove("x")
}
