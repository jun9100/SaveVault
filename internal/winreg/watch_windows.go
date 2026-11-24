//go:build windows

package winreg

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	regNotifyChangeName    = 0x00000001
	regNotifyChangeLastSet = 0x00000004
	regNotifyFilter        = regNotifyChangeName | regNotifyChangeLastSet
	winErrKeyDeleted       = 1018
	watchMinBackoff        = 250 * time.Millisecond
	watchMaxBackoff        = 4 * time.Second
)

var (
	advapi32DLL                 = windows.NewLazySystemDLL("advapi32.dll")
	procRegNotifyChangeKeyValue = advapi32DLL.NewProc("RegNotifyChangeKeyValue")
)

func watchKeys(ctx context.Context, paths []string, onChange func(keyPath string)) error {
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	for _, path := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := watchSingleKey(ctx, path, onChange); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return firstErr
}

// openNearestExistingKey opens targetPath for REG_NOTIFY if it exists, or
// walks up to the nearest existing ancestor under the same hive so creating a
// brand-new game key still triggers a notification without polling.
func openNearestExistingKey(hive registry.Key, sub string) (h registry.Key, exact bool, err error) {
	cur := strings.Trim(sub, `\`)
	isExact := true
	for {
		k, openErr := registry.OpenKey(hive, cur, registry.NOTIFY)
		if openErr == nil {
			return k, isExact, nil
		}
		if !errors.Is(openErr, registry.ErrNotExist) || cur == "" {
			return 0, false, openErr
		}
		isExact = false
		if idx := strings.LastIndex(cur, `\`); idx > 0 {
			cur = cur[:idx]
		} else {
			cur = ""
		}
	}
}

func watchSingleKey(ctx context.Context, path string, onChange func(keyPath string)) error {
	// Win32 cancels async RegNotifyChangeKeyValue registrations if the
	// registering OS thread exits, so pin this goroutine to one OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hiveName, sub, err := splitHive(path)
	if err != nil {
		return err
	}
	root := hiveKeys[hiveName]

	stopEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return fmt.Errorf("create registry stop event: %w", err)
	}
	stopCtx, stopCancel := context.WithCancel(ctx)
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		<-stopCtx.Done()
		_ = windows.SetEvent(stopEvent)
	}()
	defer func() {
		stopCancel()
		<-stopDone
		_ = windows.CloseHandle(stopEvent)
	}()

	changeEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return fmt.Errorf("create registry change event: %w", err)
	}
	defer func() { _ = windows.CloseHandle(changeEvent) }()

	handles := []windows.Handle{stopEvent, changeEvent}
	backoff := watchMinBackoff

	for ctx.Err() == nil {
		keyHandle, exact, openErr := openNearestExistingKey(root, sub)
		if openErr != nil {
			if !sleepContext(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, watchMaxBackoff)
			continue
		}
		if exact {
			backoff = watchMinBackoff
		}

		for ctx.Err() == nil {
			_ = windows.ResetEvent(changeEvent)
			r0, _, _ := procRegNotifyChangeKeyValue.Call(
				uintptr(keyHandle),
				1, // bWatchSubtree = TRUE
				uintptr(regNotifyFilter),
				uintptr(changeEvent),
				1, // fAsynchronous = TRUE
			)
			if r0 != 0 {
				if r0 == winErrKeyDeleted && exact && onChange != nil && ctx.Err() == nil {
					onChange(path)
				}
				break
			}

			waitRes, waitErr := windows.WaitForMultipleObjects(handles, false, windows.INFINITE)
			if waitErr != nil {
				_ = keyHandle.Close()
				return fmt.Errorf("wait registry event %q: %w", path, waitErr)
			}
			if waitRes == windows.WAIT_OBJECT_0 || ctx.Err() != nil {
				_ = keyHandle.Close()
				return nil
			}
			if waitRes == windows.WAIT_OBJECT_0+1 {
				if exact {
					if onChange != nil {
						onChange(path)
					}
					continue
				}
				// Watching a broad ancestor (e.g. HKCU\Software): only fire
				// onChange once the exact target key actually appears, and
				// break to reopen directly on the target key.
				if target, err := registry.OpenKey(root, sub, registry.NOTIFY); err == nil {
					_ = target.Close()
					if onChange != nil {
						onChange(path)
					}
					backoff = watchMinBackoff
					break
				}
				// Target still absent: rate-limit re-arming on noisy ancestors.
				if !sleepContext(ctx, backoff) {
					_ = keyHandle.Close()
					return nil
				}
				backoff = min(backoff*2, watchMaxBackoff)
			}
		}
		_ = keyHandle.Close()
		if !sleepContext(ctx, backoff) {
			return nil
		}
	}
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
