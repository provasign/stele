// stele: checks spec registries for rules that cannot all hold.
package main

import (
	"os"
	"path/filepath"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/cli"
	"github.com/provasign/stele/internal/solver"
)

func main() {
	checker.PersistDir = cacheDir()
	solver.DefaultWarm = os.Getenv("STELE_WARM_Z3") != "off"
	os.Exit(cli.Run(os.Args[1:]))
}

// cacheDir is where checker results persist between runs: STELE_CACHE_DIR,
// or the user cache directory. STELE_CACHE=off keeps none.
func cacheDir() string {
	if os.Getenv("STELE_CACHE") == "off" {
		return ""
	}
	if d := os.Getenv("STELE_CACHE_DIR"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "stele", "checker")
}
