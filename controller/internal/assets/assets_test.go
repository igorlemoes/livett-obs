// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package assets

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedPanelAndBridgeArePresent(t *testing.T) {
	for _, name := range []string{"index.html", "app.js", "style.css", "live.html", "live.js", "live.css"} {
		data, err := fs.ReadFile(Web(), name)
		if err != nil || len(data) == 0 {
			t.Errorf("%s is missing from the embedded panel: %v", name, err)
		}
	}
	index, _ := fs.ReadFile(Web(), "index.html")
	if !strings.Contains(string(index), "__PANEL_TOKEN__") {
		t.Error("the panel page must carry the token placeholder the server replaces")
	}
	js := PanelJS()
	if !strings.Contains(js, "__tiktokPanelV9") || len(js) < 1000 {
		t.Error("the browser bridge script is missing or incomplete")
	}
}

func TestEmbeddedCameraPagesAreStrictAboutScripts(t *testing.T) {
	for _, name := range []string{"phone.html", "phone.js", "rx.html", "rx.js", "rx.css", "camera.css"} {
		if data, err := fs.ReadFile(Camera(), name); err != nil || len(data) == 0 {
			t.Errorf("%s is missing from the camera pages: %v", name, err)
		}
	}
	// The pages run under a policy without inline scripts or styles: nothing may depend on them.
	for _, name := range []string{"phone.html", "rx.html"} {
		page, _ := fs.ReadFile(Camera(), name)
		text := string(page)
		if strings.Contains(text, "<style") || strings.Contains(text, " style=") || strings.Contains(text, "onclick=") {
			t.Errorf("%s has inline style or handlers that the page policy blocks", name)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "<script") && !strings.Contains(line, "src=") {
				t.Errorf("%s has an inline script", name)
			}
		}
	}
}
