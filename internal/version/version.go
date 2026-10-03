// Package version is tower's release version, stamped at build time.
package version

// Version is set with -ldflags "-X github.com/jerrykal/tower/internal/version.Version=…":
// the tag for a release ("0.0.1"), or the last tag with -dev+<commit> for a
// development build. A plain `go build` leaves the placeholder.
var Version = "0.0.0-dev"
