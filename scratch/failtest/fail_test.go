// Package failtest is a throwaway test package for the T-1 CI negative check. Do not merge.
package failtest

import "testing"

func TestInjectedFailure(t *testing.T) {
	t.Fatal("injected")
}
