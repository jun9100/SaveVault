//go:build !windows

package winreg

import "context"

func watchKeys(ctx context.Context, _ []string, _ func(keyPath string)) error {
	<-ctx.Done()
	return nil
}
