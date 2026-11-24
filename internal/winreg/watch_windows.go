//go:build windows

package winreg

import (
	"context"
	"errors"
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
	watchRetryBackoff      = 300 * time.Millisecond
)

var (
	advapi32DLL                 = windows.NewLazySystemDLL("advapi32.dll")
	procRegNotifyChangeKeyValue = advapi32DLL.NewProc("RegNotifyChangeKeyValue")
)

func watchSupported() bool { return true }

func watchKeys(ctx context.Context, paths []string, onChange func(keyPath string)) error {
	var wg sync.WaitGroup
	for _, p := range paths {
		path := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			watchSingleKey(ctx, path, onChange)
		}()
	}
	wg.Wait()
	return nil
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

func watchSingleKey(ctx context.Context, path string, onChange func(keyPath string)) {
	hiveName, sub, err := splitHive(path)
	if err != nil {
		return
	}
	root, ok := hiveKeys[hiveName]
	if !ok {
		return
	}

	// Create one manual-reset stop event bridged to ctx.Done(), and ensure the
	// bridge goroutine has exited before closing the handle.
	stopEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return
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
		return
	}
	defer func() { _ = windows.CloseHandle(changeEvent) }()

	handles := []windows.Handle{stopEvent, changeEvent}

	for ctx.Err() == nil {
		keyHandle, exact, openErr := openNearestExistingKey(root, sub)
		if openErr != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(watchRetryBackoff):
				continue
			}
		}

		reopen := false
		for !reopen && ctx.Err() == nil {
			r0, _, _ := procRegNotifyChangeKeyValue.Call(
				uintptr(keyHandle),
				1, // bWatchSubtree = TRUE
				uintptr(regNotifyFilter),
				uintptr(changeEvent),
				1, // fAsynchronous = TRUE
			)
			if r0 != 0 {
				if r0 == winErrKeyDeleted && onChange != nil && ctx.Err() == nil {
					onChange(path)
				}
				reopen = true
				break
			}

			waitRes, waitErr := windows.WaitForMultipleObjects(handles, false, windows.INFINITE)
			if waitErr != nil || waitRes == windows.WAIT_OBJECT_0 {
				// Context cancelled (stopEvent fired) or wait error.
				_ = keyHandle.Close()
				return
			}
			if waitRes == windows.WAIT_OBJECT_0+1 {
				if ctx.Err() != nil {
					_ = keyHandle.Close()
					return
				}
				if onChange != nil {
					onChange(path)
				}
				// If we were watching an ancestor because target didn't exist
				// yet, reopen so we attach directly to the target once created.
				if !exact {
					reopen = true
				}
			}
		}
		_ = keyHandle.Close()
		if ctx.Err() != nil {
			return
		}
		if reopen {
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}
