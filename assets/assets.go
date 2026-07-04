package assets

import "embed"

// FS contains shared static assets used by local development tools.
//
//go:embed spindle.glb
var FS embed.FS
