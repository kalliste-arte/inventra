//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

func findBrowser() string {
	var cands []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		cands = append(cands,
			filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(base, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
		)
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// openWindow shows the app in its own window (Edge/Chrome "app mode": no tabs,
// no address bar). Returns true if it waited for that window to close.
func openWindow(url, dataDir string) bool {
	if b := findBrowser(); b != "" {
		profile := filepath.Join(dataDir, "window-profile")
		cmd := exec.Command(b,
			"--app="+url,
			"--user-data-dir="+profile,
			"--window-size=1440,900",
			"--no-first-run",
			"--no-default-browser-check",
			"--disable-features=Translate",
			"--disable-background-mode",
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		start := time.Now()
		if err := cmd.Start(); err == nil {
			cmd.Wait()
			if time.Since(start) > 4*time.Second {
				return true
			}
			// The browser handed the window to an already-running copy;
			// fall back to heartbeat-based shutdown.
			return false
		} else {
			log.Printf("browser launch failed: %v", err)
		}
	}
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	return false
}

func fatal(msg string) {
	log.Print("FATAL: " + msg)
	user32 := syscall.NewLazyDLL("user32.dll")
	box := user32.NewProc("MessageBoxW")
	t, _ := syscall.UTF16PtrFromString(appName)
	m, _ := syscall.UTF16PtrFromString(msg)
	box.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10)
	os.Exit(1)
}
