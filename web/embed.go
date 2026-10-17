// Package web holds the embedded frontend, served as-is: no build step, no
// templating (§2, §3).
package web

import "embed"

// sw.js must be embedded at the root and served from /sw.js, never from a
// nested path. Worker scope is path-based, and a nested worker cannot control
// the page (§9.5).
//
// The manifest is required, not optional (§9.3). The Badging API only works
// in an installed PWA, and on iPhone, Web Push only works from the Home
// Screen. icons/ holds exactly the four PNGs that §9.3 names. Their sources
// are in docs/icons/.
//
//go:embed index.html app.css app.js sw.js manifest.webmanifest icons
var Files embed.FS
