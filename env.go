package stellarjay

import (
	"log/slog"
	"os"
	"strings"
	"sync"
)

// legacyPrefix is the environment prefix used before the Stellar Jay rename.
const legacyPrefix = "JAYBASE_"

var warnedLegacy sync.Map

// Getenv returns the trimmed value of a STELLARJAY_ variable. When it is unset,
// the pre-rename STELLARJAY_ name is read instead and a one-time deprecation
// warning is logged, so existing deployments keep working.
func Getenv(name string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	rest, ok := strings.CutPrefix(name, "STELLARJAY_")
	if !ok {
		return ""
	}
	legacy := legacyPrefix + rest
	value := strings.TrimSpace(os.Getenv(legacy))
	if value != "" {
		if _, seen := warnedLegacy.LoadOrStore(legacy, true); !seen {
			slog.Warn("deprecated environment variable; rename it", "name", legacy, "use", name)
		}
	}
	return value
}

// DefaultDir is the local store directory used when none is given. A store
// created before the rename under .stellarjay keeps being used.
func DefaultDir() string {
	if _, err := os.Stat(".stellarjay"); err != nil {
		if info, legacyErr := os.Stat(".jaybase"); legacyErr == nil && info.IsDir() {
			return ".jaybase"
		}
	}
	return ".stellarjay"
}
