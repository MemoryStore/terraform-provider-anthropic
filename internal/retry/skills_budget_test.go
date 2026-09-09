// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package retry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestBudgetAdmissionRechecksPacingAndExtendedCooldown(t *testing.T) {
	now := time.Unix(100, 0)
	s := budgetState{}
	if delay, ok := s.admit(now, time.Second); delay != 0 || !ok {
		t.Fatal(delay, ok)
	}
	if delay, ok := s.admit(now.Add(400*time.Millisecond), time.Second); delay != 600*time.Millisecond || ok {
		t.Fatal(delay, ok)
	}
	// A second caller's 429 extends the wait of an already waiting first caller.
	s.Cooldown = now.Add(3 * time.Second).UnixNano()
	if delay, ok := s.admit(now.Add(time.Second), time.Second); delay != 2*time.Second || ok {
		t.Fatal(delay, ok)
	}
	if s.Next != now.Add(time.Second).UnixNano() {
		t.Fatal("waiting reserved a future slot")
	}
	if delay, ok := s.admit(now.Add(3*time.Second), time.Second); delay != 0 || !ok {
		t.Fatal(delay, ok)
	}
	if delay, ok := s.admit(now.Add(3*time.Second), time.Second); delay != time.Second || ok {
		t.Fatal(delay, ok)
	}
}

func TestConfiguredClientsShareBudget(t *testing.T) {
	t.Setenv(skillsRPMEnv, "900")
	t.Setenv(skillsFileEnv, filepath.Join(t.TempDir(), "budget.json"))
	a, err := NewHTTPClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewHTTPClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Transport.(*rateLimitTransport).skillsLimiter != b.Transport.(*rateLimitTransport).skillsLimiter {
		t.Fatal("clients have separate budgets")
	}
}

func TestConfiguredBudgetFailsClosed(t *testing.T) {
	for _, rpm := range []string{"", "0", "-1", "NaN", "Inf", "garbage"} {
		t.Run("rpm="+rpm, func(t *testing.T) {
			t.Setenv(skillsRPMEnv, rpm)
			if _, err := NewHTTPClient(context.Background()); err == nil {
				t.Fatal("invalid RPM accepted")
			}
		})
	}
	t.Setenv(skillsRPMEnv, "90")
	for _, path := range []string{"", "relative.json", filepath.Join(t.TempDir(), "missing", "budget.json")} {
		t.Run("path="+path, func(t *testing.T) {
			t.Setenv(skillsFileEnv, path)
			if _, err := NewHTTPClient(context.Background()); err == nil {
				t.Fatal("invalid coordination path accepted")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "budget.json")
	t.Setenv(skillsFileEnv, path)
	for _, data := range []string{"", "{}", "null", "not json", `{"version":2}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewHTTPClient(context.Background()); err == nil {
			t.Fatalf("accepted invalid state %q", data)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHTTPClient(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(skillsRPMEnv, "45")
	if _, err := NewHTTPClient(context.Background()); err == nil {
		t.Fatal("conflicting shared budget accepted")
	}
}

func TestFileLockWaitCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := acquireFileLock(context.Background(), lock); err != nil {
		t.Fatal(err)
	}
	defer releaseFileLock(lock)
	l := newSerialLimiter(time.Second)
	l.path = path
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait = %v", err)
	}
}

func TestSkills429PublishesCooldownForSiblingAndMultipart(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		t.Run(fmt.Sprint(multipart), func(t *testing.T) {
			l := newSerialLimiter(0)
			var calls atomic.Int32
			transport := &rateLimitTransport{skillsLimiter: l, base: handlerTransport(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(429)
			}))}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "https://anthropic.test/v1/skills/x", nil)
			if multipart {
				req.Method = http.MethodPost
				req.Body = http.NoBody
				req.GetBody = nil
			}
			res, err := transport.RoundTrip(req)
			if multipart {
				if err != nil || res.StatusCode != 429 {
					t.Fatal(res, err)
				}
				res.Body.Close()
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			siblingCtx, siblingCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer siblingCancel()
			if err := l.wait(siblingCtx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("sibling ignored cooldown", err)
			}
			if calls.Load() != 1 {
				t.Fatal("unexpected calls", calls.Load())
			}
		})
	}
}

func TestOtherEndpointsAndOrdinary4xxRemainUnaffected(t *testing.T) {
	l := newSerialLimiter(time.Hour)
	if err := l.deferFor(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	tr := &rateLimitTransport{skillsLimiter: l, base: handlerTransport(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(400) }))}
	req, _ := http.NewRequest(http.MethodDelete, "https://anthropic.test/v1/agents/a", nil)
	res, err := tr.RoundTrip(req)
	if err != nil || res.StatusCode != 400 || calls.Load() != 1 {
		t.Fatal(res, err, calls.Load())
	}
	res.Body.Close()
	l = newSerialLimiter(0)
	tr.skillsLimiter = l
	req.URL.Path = "/v1/skills/x"
	res, err = tr.RoundTrip(req)
	if err != nil || res.StatusCode != 400 || calls.Load() != 2 {
		t.Fatal(res, err, calls.Load())
	}
	res.Body.Close()
}

func TestSharedBudgetHelperProcess(t *testing.T) {
	if os.Getenv("SKILLS_BUDGET_HELPER") != "1" {
		return
	}
	l := newSerialLimiter(50 * time.Millisecond)
	l.path = os.Getenv("SKILLS_BUDGET_TEST_PATH")
	if os.Getenv("SKILLS_BUDGET_ACTION") == "cooldown" {
		if err := l.deferFor(context.Background(), 250*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		return
	}
	for range 3 {
		if err := l.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIndependentProcessesSharePacingAndCooldown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	helper := func(action string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSharedBudgetHelperProcess$")
		cmd.Env = append(os.Environ(), "SKILLS_BUDGET_HELPER=1", "SKILLS_BUDGET_TEST_PATH="+path, "SKILLS_BUDGET_ACTION="+action)
		return cmd
	}
	// A distinct process announces the cooldown before both consumers start.
	if out, err := helper("cooldown").CombinedOutput(); err != nil {
		t.Fatalf("cooldown: %s %v", out, err)
	}
	initialData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var initial budgetState
	if err := json.Unmarshal(initialData, &initial); err != nil {
		t.Fatal(err)
	}
	commands := []*exec.Cmd{helper("wait"), helper("wait")}
	for _, cmd := range commands {
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var final budgetState
	if err := json.Unmarshal(data, &final); err != nil {
		t.Fatal(err)
	}
	// Six admissions must advance one shared clock by six intervals after cooldown.
	if final.Next < initial.Cooldown+int64(6*50*time.Millisecond) {
		t.Fatalf("independent processes bypassed shared budget: %+v -> %+v", initial, final)
	}
}
