// Package web embeds the dashboard so the panel ships as a single binary.
package web

import "embed"

//go:embed static
var Static embed.FS
