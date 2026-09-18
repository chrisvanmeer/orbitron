// Package build carries build-time metadata stamped into the binary via
// -ldflags "-X orbitron/internal/build.Version=...". The value is reported by
// `orbitron --version` so automation can detect and manage upgrades.
package build

// Version is the release version of this build, e.g. "v0.4.0". It defaults to
// "dev" for local builds compiled without an explicit -ldflags stamp.
var Version = "dev"
