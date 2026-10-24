//go:build !windows

package main

import (
	"log"
	"os"
	"os/exec"
	"runtime"
)

func openWindow(url, dataDir string) bool {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
	log.Printf("Open %s in your browser", url)
	return false
}

func fatal(msg string) {
	log.Print("FATAL: " + msg)
	os.Exit(1)
}
