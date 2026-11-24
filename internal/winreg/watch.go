package winreg

import (
	"context"
	"strings"
)

// WatchKeys watches the given Windows registry key paths (recursively) and
// invokes onChange whenever a key, subkey, or value under any of them changes.
// It blocks until ctx is cancelled, returning nil on clean cancellation.
//
// On non-Windows platforms, WatchKeys simply blocks until ctx.Done() and
// returns nil.
func WatchKeys(ctx context.Context, paths []string, onChange func(keyPath string)) error {
	cleaned := normalizeWatchPaths(paths)
	if len(cleaned) == 0 {
		<-ctx.Done()
		return nil
	}
	return watchKeys(ctx, cleaned, onChange)
}

func normalizeWatchPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		norm := NormalizePath(raw)
		if norm == "" {
			continue
		}
		key := strings.ToUpper(norm)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, norm)
	}
	return out
}
