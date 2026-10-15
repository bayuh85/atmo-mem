// Package version holds the atmo-mem build version.
// Release builds override Version with -ldflags.
package version

// Version is the semantic version printed by the CLI and sent as a User-Agent.
// GoReleaser replaces it for tagged releases.
var Version = "dev"
