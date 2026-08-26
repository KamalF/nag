// Package web holds the embedded frontend, served as-is — no build step,
// no templating (§2, §3).
package web

import "embed"

// The manifest is required rather than optional (§9.3): the Badging API only
// works in an installed PWA, and on iPhone Web Push only works from the Home
// Screen. icons/ holds exactly the four PNGs §9.3 names — sources in
// docs/icons/.
//
//go:embed index.html app.js manifest.webmanifest icons
var Files embed.FS
