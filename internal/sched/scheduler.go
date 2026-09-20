// Package sched runs recurring restart and backup jobs for server instances.
// It supports both interval ("every 6h") and wall clock ("at 04:00") schedules.
package sched

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Runnable is what the scheduler acts on: a server instance.
type Runnable interface {
	ID() string
	Schedule() *Schedule
	Restart(ctx context.Context) error
	Backup(ctx context.Context) (string, error)
	Say(msg string) error
}

// Schedule is duplicated from config to keep this package free of dependencies
// on the instance model; the manager converts between them.
type Schedule struct {
	RestartEvery string
	BackupEvery  string
	RestartAt    string
	BackupAt     string
}

// Scheduler owns one goroutine per scheduled instance.
type Scheduler struct {
	mu      sync.Mutex
	runners map[string]Runnable
	cancels map[string]context.CancelFunc
	logger  *slog.Logger
}

// New returns a stopped scheduler.
func New(logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		runners: make(map[string]Runnable),
		cancels: make(map[string]context.CancelFunc),
		logger:  logger,
	}
}

// Set registers (or replaces) the runner for one instance and (re)starts its
// loop. Call this whenever an instance is created or its schedule changes.
func (s *Scheduler) Set(r Runnable) {
	if r == nil {
		return
	}
	s.mu.Lock()
	if cancel, ok := s.cancels[r.ID()]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.runners[r.ID()] = r
	s.cancels[r.ID()] = cancel
	s.mu.Unlock()

	go s.loop(ctx, r)
}

// Remove stops and forgets an instance's jobs.
func (s *Scheduler) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
		delete(s.runners, id)
	}
}

// loop waits for the next due job, runs it, then recomputes.
func (s *Scheduler) loop(ctx context.Context, r Runnable) {
	for {
		if ctx.Err() != nil {
			return
		}
		sc := r.Schedule()
		now := time.Now()

		var restartAt, backupAt time.Time
		if sc != nil {
			if t, ok := nextRun(now, sc.RestartEvery, sc.RestartAt); ok {
				restartAt = t
			}
			if t, ok := nextRun(now, sc.BackupEvery, sc.BackupAt); ok {
				backupAt = t
			}
		}
		next := earliest(restartAt, backupAt)
		if next.IsZero() {
			// Nothing scheduled; re-check the config periodically so changes
			// are picked up without a manager round trip.
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}

		delay := time.Until(next)
		if delay < 0 {
			delay = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if ctx.Err() != nil {
			return
		}

		if !restartAt.IsZero() && !time.Now().Before(restartAt) {
			if err := r.Say("scheduled restart in 1 minute"); err == nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Minute):
				}
			}
			s.logger.Info("scheduled restart", "server", r.ID())
			if err := r.Restart(ctx); err != nil {
				s.logger.Warn("scheduled restart failed", "server", r.ID(), "err", err)
			}
		}
		if !backupAt.IsZero() && !time.Now().Before(backupAt) {
			s.logger.Info("scheduled backup", "server", r.ID())
			if _, err := r.Backup(ctx); err != nil {
				s.logger.Warn("scheduled backup failed", "server", r.ID(), "err", err)
			}
		}
	}
}

// nextRun computes when a job fires next, given an interval and/or a
// wall clock time. At least one of every / at must be set.
func nextRun(now time.Time, every, at string) (time.Time, bool) {
	if every != "" {
		if d, err := parseDuration(every); err == nil {
			return now.Add(d), true
		}
	}
	if at != "" {
		if t, err := parseClock(now, at); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseDuration accepts Go durations and a few human shortcuts (e.g. "6h",
// "30m", "1d").
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		n, err := parseInt(days)
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// parseClock returns the next occurrence of "HH:MM" after now.
func parseClock(now time.Time, s string) (time.Time, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("invalid time %q", s)
	}
	hour, err := parseInt(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return time.Time{}, fmt.Errorf("invalid hour in %q", s)
	}
	minute, err := parseInt(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return time.Time{}, fmt.Errorf("invalid minute in %q", s)
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t, nil
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}

func earliest(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if a.Before(b) {
		return a
	}
	return b
}
