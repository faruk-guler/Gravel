package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"gravel/internal/sessionstore"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()
	// WebView2 stores localStorage in a folder named after the executable unless
	// told otherwise, so the previous renames silently orphaned the saved theme,
	// font size and pane width. Pin the profile to the app's own config folder -
	// which does carry older data forward - and adopt any leftover per-executable
	// profile before the webview is created.
	userData := ""
	if dir, err := sessionstore.DefaultDir(); err == nil {
		sessionstore.AdoptWebviewProfile(dir)
		userData = dir
	}
	err := wails.Run(&options.App{
		Title:            "Gravel",
		Width:            1280,
		Height:           800,
		MinWidth:         900,
		MinHeight:        600,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 17, G: 17, B: 17, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		// WebView2's built-in zoom stays off (IsZoomControlEnabled defaults to
		// false) and pinch zoom is switched off too: Wails exposes no
		// MinimumZoomFactor/MaximumZoomFactor, so the webview would zoom without
		// bounds. Gravel deliberately offers no zoom at all - resizing the
		// terminal widget without the remote acknowledging the new size garbles
		// line wrapping - see ZOOM_KEY in frontend/src/main.js.
		Windows: &windows.Options{
			DisablePinchZoom: true,
			// Empty would fall back to %APPDATA%\[BinaryName.exe], which is what
			// made the stored preferences disappear on every rename.
			WebviewUserDataPath: userData,
		},
	})
	if err != nil {
		println("Gravel error:", err.Error())
	}
}
