//go:build !windows

package winreg

import "context"

func watchSupported() bool { return false }

func watchKeys(ctx context.Context, _ []string, _ func(keyPath string)) error {
	<-ctx.Done()
	return nil
}
