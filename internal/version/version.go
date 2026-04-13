// Package version holds the embedded aidev version string. The value
// is set at build time via `-ldflags "-X .../version.Version=v0.2f"`
// by the release workflow; development builds default to "dev".
//
// Consumers: `aidev --version`, the self-update subcommand (for
// comparing the installed binary's version against the latest
// release), and the audit trail (so the controller can stamp
// comments with the aidev version that produced them — useful for
// debugging drift across runs).
package version

// Version is the embedded version string. Set via -ldflags in the
// release workflow. Defaults to "dev" so local `go build` produces
// a recognizable marker without requiring a tag.
var Version = "dev"
