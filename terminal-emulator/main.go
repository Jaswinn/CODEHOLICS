package main

import (
	"embed"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var frontend embed.FS

func main() {
	assets, err := fs.Sub(frontend, "frontend/dist")
	if err != nil {
		log.Fatal(err)
	}

	app := NewApp()
	err = wails.Run(&options.App{
		Title:                    "CodeHolic Terminal",
		Width:                    1440,
		Height:                   900,
		MinWidth:                 760,
		MinHeight:                520,
		AssetServer:              &assetserver.Options{Assets: assets},
		OnStartup:                app.startup,
		OnShutdown:               app.shutdown,
		Bind:                     []interface{}{app},
		EnableDefaultContextMenu: true,
	})
	if err != nil {
		log.Fatal(err)
	}
}
