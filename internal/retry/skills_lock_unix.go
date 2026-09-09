//go:build !windows

// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package retry

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

func acquireFileLock(ctx context.Context, file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if err := waitForRetry(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
}

func releaseFileLock(file *os.File) { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }
