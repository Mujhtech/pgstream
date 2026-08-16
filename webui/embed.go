// Package webui embeds the built web interface so the pgstream binary can
// serve it without a separate deployment. Run `npm run build` (or
// `make webui`) in this directory before building the Go binary to refresh
// the embedded assets.
package webui

import "embed"

//go:embed all:dist
var Dist embed.FS
