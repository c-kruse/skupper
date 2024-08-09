package main

import (
	"embed"
	"io/fs"
)

//go:embed spec/*
var specDir embed.FS

func getSpecContent() (fs.FS, error) {
	return fs.Sub(fs.FS(specDir), "spec")
}
