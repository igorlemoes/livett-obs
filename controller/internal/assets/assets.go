// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package assets embeds the web panel, the wireless camera pages and the browser bridge script, so the controller is a
// single executable.
package assets

import (
	"embed"
	"io/fs"
)

//go:embed web/* camera/* panel_browser.js
var files embed.FS

// Web is the panel (index.html, app.js, style.css, live.html, live.js, live.css).
func Web() fs.FS {
	sub, err := fs.Sub(files, "web")
	if err != nil {
		panic(err)
	}
	return sub
}

// Camera is the wireless camera: the phone's page (phone.html, phone.js), the OBS receiver (rx.html, rx.js, rx.css) and camera.css.
func Camera() fs.FS {
	sub, err := fs.Sub(files, "camera")
	if err != nil {
		panic(err)
	}
	return sub
}

// PanelJS is the script injected into the service tab of the managed Chrome.
func PanelJS() string {
	b, err := files.ReadFile("panel_browser.js")
	if err != nil {
		panic(err)
	}
	return string(b)
}
