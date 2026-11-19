// Package version holds the build information of the Kybernate binaries.
package version

import "fmt"

// Set at build time through -ldflags.
var (
	Version = "dev"
	Commit  = "unknown"
)

// String returns the version and commit in one line.
func String() string {
	return fmt.Sprintf("%s (commit %s)", Version, Commit)
}
