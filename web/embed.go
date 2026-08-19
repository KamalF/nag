// Package web holds the embedded frontend, served as-is — no build step,
// no templating (§2, §3).
package web

import "embed"

//go:embed index.html
var Files embed.FS
