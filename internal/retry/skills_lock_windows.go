// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package retry

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func acquireFileLock(ctx context.Context, file *os.File) error {
	for {
		err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		if err := waitForRetry(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
}

func releaseFileLock(file *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}
