/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package proxy

import (
	"context"
	"errors"
	"sync"
	"time"
)

// targetHealth holds the most recent probe result.
type targetHealth struct {
	mu        sync.RWMutex
	checked   bool
	reachable bool
	at        time.Time
	err       string
}

// targetHealthSnapshot is a consistent view of targetHealth for a caller to
// report without holding the lock.
type targetHealthSnapshot struct {
	checked   bool
	reachable bool
	at        time.Time
	err       string
}

func (h *targetHealth) record(reachable bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.checked = true
	h.reachable = reachable
	h.at = time.Now()
	h.err = ""
	if err != nil {
		h.err = err.Error()
	}
}

func (h *targetHealth) snapshot() targetHealthSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return targetHealthSnapshot{
		checked:   h.checked,
		reachable: h.reachable,
		at:        h.at,
		err:       h.err,
	}
}

// probeOnce probes the target and records the result, so the reported state and
// the metric are both written by whatever drives the probe rather than by an
// incoming request.
func (s *Server) probeOnce(ctx context.Context) {
	target := s.config.TargetAddr()

	err := probeTarget(ctx, target, s.config.TargetHealthBannerPrefix)

	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}

	s.targetHealth.record(err == nil, err)

	if err != nil {
		s.metrics.TargetReachable.Set(0)
		s.logger.V(1).Info("Target probe failed", "target", target, "error", err.Error())
		return
	}
	s.metrics.TargetReachable.Set(1)
}

// startTargetProber begins probing the target on a fixed interval. No-op after Shutdown.
func (s *Server) startTargetProber() {
	interval := s.config.TargetHealthInterval
	if interval <= 0 {
		s.logger.Info("Target health interval is not positive, using the default",
			"configured", interval, "using", defaultTargetHealthInterval)
		interval = defaultTargetHealthInterval
	}

	s.proberMu.Lock()
	if s.proberStopped || s.proberDone != nil {
		s.proberMu.Unlock()
		return
	}
	done := make(chan struct{})
	s.proberDone = done
	s.proberMu.Unlock()

	go func() {
		defer close(done)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			probeCtx, probeCancel := context.WithTimeout(s.proberCtx, targetHealthDialTimeout)
			s.probeOnce(probeCtx)
			probeCancel()

			select {
			case <-s.proberCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// stopTargetProber ends probing and waits for the goroutine to return. It is safe
// to call when no prober was started.
func (s *Server) stopTargetProber() {
	s.proberMu.Lock()
	if s.proberStopped {
		s.proberMu.Unlock()
		return
	}
	s.proberStopped = true
	done := s.proberDone
	s.proberMu.Unlock()

	s.proberCancel()

	if done != nil {
		<-done
	}
}
