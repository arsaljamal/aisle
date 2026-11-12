package server

import (
	"embed"
	"io/fs"
)

//go:embed ui
var ui embed.FS

func uiFS() fs.FS {
	sub, _ := fs.Sub(ui, "ui")
	return sub
}
