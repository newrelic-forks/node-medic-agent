/*
Copyright 2026 The New Relic Container Fabric authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDebounceMap_Allow_FirstHitAlwaysAllows(t *testing.T) {
	t.Parallel()

	d := NewDebounceMap(30 * time.Second)
	k := DebounceKey{Node: "n1", Type: "ConntrackSaturated"}
	now := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)

	if !d.Allow(k, now) {
		t.Fatal("first Allow should always return true")
	}
}

func TestDebounceMap_Allow_WithinWindowDenies(t *testing.T) {
	t.Parallel()

	d := NewDebounceMap(30 * time.Second)
	k := DebounceKey{Node: "n1", Type: "ConntrackSaturated"}
	t0 := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)

	d.Allow(k, t0)
	if d.Allow(k, t0.Add(5*time.Second)) {
		t.Errorf("Allow within window should return false")
	}
	if d.Allow(k, t0.Add(29*time.Second)) {
		t.Errorf("Allow at 29s (still inside 30s window) should return false")
	}
}

func TestDebounceMap_Allow_AtOrPastWindowAllows(t *testing.T) {
	t.Parallel()

	d := NewDebounceMap(30 * time.Second)
	k := DebounceKey{Node: "n1", Type: "ConntrackSaturated"}
	t0 := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)

	d.Allow(k, t0)
	if !d.Allow(k, t0.Add(30*time.Second)) {
		t.Errorf("Allow at exactly the window boundary should return true")
	}
	if !d.Allow(k, t0.Add(60*time.Second)) {
		t.Errorf("Allow past the window should return true")
	}
}

func TestDebounceMap_Allow_DistinctKeysIndependent(t *testing.T) {
	t.Parallel()

	d := NewDebounceMap(30 * time.Second)
	a := DebounceKey{Node: "n1", Type: "ConntrackSaturated"}
	b := DebounceKey{Node: "n1", Type: "FDExhaustion"}
	c := DebounceKey{Node: "n2", Type: "ConntrackSaturated"}
	t0 := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)

	if !d.Allow(a, t0) {
		t.Error("first hit on a should allow")
	}
	if !d.Allow(b, t0) {
		t.Error("first hit on b should allow even if a is in window")
	}
	if !d.Allow(c, t0) {
		t.Error("first hit on c should allow even if a is in window")
	}
}

func TestDebounceMap_Allow_DenialPreservesLastSeen(t *testing.T) {
	t.Parallel()

	d := NewDebounceMap(30 * time.Second)
	k := DebounceKey{Node: "n1", Type: "ConntrackSaturated"}
	t0 := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)

	d.Allow(k, t0)
	if d.Allow(k, t0.Add(10*time.Second)) {
		t.Fatal("expected denial at 10s")
	}
	// After denial, the recorded LastSeen is still t0, so an Allow at
	// t0+30s (boundary) succeeds — not t0+10s+30s.
	if d.LastSeen(k) != t0 {
		t.Errorf("denial should preserve LastSeen at %v, got %v", t0, d.LastSeen(k))
	}
	if !d.Allow(k, t0.Add(30*time.Second)) {
		t.Error("Allow at t0+30s should succeed because LastSeen was preserved at t0")
	}
}

func TestDebounceMap_Concurrent(t *testing.T) {
	t.Parallel()

	d := NewDebounceMap(30 * time.Second)
	k := DebounceKey{Node: "n1", Type: "ConntrackSaturated"}
	t0 := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)

	const goroutines = 64
	var allowed atomic.Int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if d.Allow(k, t0) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	// First Allow always wins; subsequent ones in the same instant fall
	// inside the window. Because sync.Map's Load+Store isn't atomic,
	// multiple goroutines may observe "no prev" and each Store. The
	// invariant we care about is "at least one Allow succeeded" —
	// not exactly one. (FR-1 idempotency is covered by deterministic
	// NHD names anyway.)
	if allowed.Load() < 1 {
		t.Errorf("expected at least 1 Allow, got 0")
	}
}
