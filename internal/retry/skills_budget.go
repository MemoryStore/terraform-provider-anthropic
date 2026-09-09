// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package retry

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	skillsRPMEnv  = "ANTHROPIC_SKILLS_REQUESTS_PER_MINUTE"
	skillsFileEnv = "ANTHROPIC_SKILLS_RATE_LIMIT_FILE"
)

type budgetState struct {
	Version  int   `json:"version"`
	Interval int64 `json:"interval_ns"`
	Next     int64 `json:"next_start_ns"`
	Cooldown int64 `json:"cooldown_until_ns"`
}

type serialLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	path     string
	state    budgetState
}

var processBudgets = struct {
	sync.Mutex
	entries map[string]*serialLimiter
}{entries: make(map[string]*serialLimiter)}

func configuredSkillsBudget(ctx context.Context) (*serialLimiter, error) {
	rpm := float64(90)
	if raw, exists := os.LookupEnv(skillsRPMEnv); exists {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
			return nil, fmt.Errorf("%s must be a positive finite number", skillsRPMEnv)
		}
		rpm = parsed
	}
	ns := float64(time.Minute) / rpm
	if ns < 1 || ns >= float64(math.MaxInt64) {
		return nil, fmt.Errorf("%s produces an unsupported request interval", skillsRPMEnv)
	}
	interval := time.Duration(math.Ceil(ns))
	path := os.Getenv(skillsFileEnv)
	if raw, exists := os.LookupEnv(skillsFileEnv); exists && (raw == "" || !filepath.IsAbs(raw)) {
		return nil, fmt.Errorf("%s must be an absolute, nonempty local file path", skillsFileEnv)
	}
	if path != "" {
		path = filepath.Clean(path)
	}
	key := fmt.Sprintf("%s:%d", path, interval)
	processBudgets.Lock()
	limiter := processBudgets.entries[key]
	if limiter == nil {
		limiter = newSerialLimiter(interval)
		limiter.path = path
		processBudgets.entries[key] = limiter
	}
	processBudgets.Unlock()
	// Validate configured coordination before handing a client to resources.
	if _, err := limiter.access(ctx, func(*budgetState) (time.Duration, bool) { return 0, false }); err != nil {
		return nil, err
	}
	return limiter, nil
}

func newSerialLimiter(interval time.Duration) *serialLimiter {
	return &serialLimiter{interval: interval, state: budgetState{Version: 1, Interval: int64(interval)}}
}

func (l *serialLimiter) wait(ctx context.Context) error {
	for {
		delay, err := l.access(ctx, func(s *budgetState) (time.Duration, bool) {
			return s.admit(time.Now(), l.interval)
		})
		if err != nil {
			return err
		}
		if delay == 0 {
			return nil
		}
		if err := waitForRetry(ctx, delay); err != nil {
			return err
		}
		// Never reserve future slots: newly announced cooldowns affect every waiter.
	}
}

// admit changes state only at actual admission, not while a caller waits.
func (s *budgetState) admit(now time.Time, interval time.Duration) (time.Duration, bool) {
	ready := time.Unix(0, max(s.Next, s.Cooldown))
	if ready.After(now) {
		return ready.Sub(now), false
	}
	s.Next = now.Add(interval).UnixNano()
	return 0, true
}

func (l *serialLimiter) deferFor(ctx context.Context, delay time.Duration) error {
	_, err := l.access(ctx, func(s *budgetState) (time.Duration, bool) {
		s.Cooldown = max(s.Cooldown, time.Now().Add(delay).UnixNano())
		return 0, true
	})
	return err
}

func (l *serialLimiter) access(ctx context.Context, change func(*budgetState) (time.Duration, bool)) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if l.path == "" {
		l.mu.Lock()
		defer l.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		delay, _ := change(&l.state)
		return delay, nil
	}
	// The lock inode is stable. Only the separate JSON state is atomically replaced.
	lock, err := os.OpenFile(l.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return 0, fmt.Errorf("skills coordination lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := acquireFileLock(ctx, lock); err != nil {
		return 0, fmt.Errorf("skills coordination lock: %w", err)
	}
	defer releaseFileLock(lock)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	state := budgetState{Version: 1, Interval: int64(l.interval)}
	data, err := os.ReadFile(l.path)
	fresh := os.IsNotExist(err)
	if err != nil && !fresh {
		return 0, fmt.Errorf("read Skills coordination state: %w", err)
	}
	if !fresh {
		state = budgetState{}
		if len(data) > 4096 || json.Unmarshal(data, &state) != nil || state.Version != 1 || state.Next < 0 || state.Cooldown < 0 {
			return 0, fmt.Errorf("invalid Skills coordination state at %s; stop all users before repairing it", l.path)
		}
		if state.Interval != int64(l.interval) {
			return 0, fmt.Errorf("skills coordination budget differs at %s; all users must configure the same RPM; stop all users before resetting state", l.path)
		}
	}
	delay, dirty := change(&state)
	if !dirty && !fresh {
		return delay, nil
	}
	data, err = json.Marshal(state)
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".skills-rate-*")
	if err != nil {
		return 0, fmt.Errorf("create Skills coordination state: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), l.path)
	}
	if err != nil {
		return 0, fmt.Errorf("persist Skills coordination state: %w", err)
	}
	return delay, nil
}
