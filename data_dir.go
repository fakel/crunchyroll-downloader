package main

import (
	"os"
	"path/filepath"
)

// defaultDataDir picks where the web interface keeps its state when --data-dir
// isn't given: $XDG_DATA_HOME/crunchyroll-downloader, or ~/.local/share/… as the
// XDG spec says. The install script passes an explicit path, so this only
// matters when the program is run by hand.
func defaultDataDir() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "crunchyroll-downloader")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "crunchyroll-downloader")
	}
	return "crunchyroll-downloader-data"
}
