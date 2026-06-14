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
	"time"
)

// DebounceMap tracks the last-seen time of each (node, conditionType)
// pair so flapping conditions don't spawn duplicate cases (FR-1
// 30 s window; data-model §6).
//
// Concurrency: the underlying sync.Map makes Allow safe under the
// controller-runtime workqueue's concurrent dispatch. There is no
// background expiry; entries are kept until the process restarts.
// That's intentional — the worst case is O(unique-pairs-ever-seen)
// in memory, which on a single test cluster is bounded by node count
// times watched-condition count (~hundreds of entries).
type DebounceMap struct {
	window time.Duration
	last   sync.Map // map[DebounceKey]time.Time
}

// NewDebounceMap returns a DebounceMap with the given window.
func NewDebounceMap(window time.Duration) *DebounceMap {
	return &DebounceMap{window: window}
}

// Window returns the configured debounce window. Exposed for logging
// and test introspection only; production code does not branch on it.
func (d *DebounceMap) Window() time.Duration { return d.window }

// Allow returns true if the (key, now) pair is outside the configured
// window from the previous Allow call for the same key. Side effect:
// records `now` as the new last-seen time. If false, the previous
// last-seen is preserved.
//
// The signature takes `now` rather than calling time.Now() so unit
// tests drive the clock deterministically.
func (d *DebounceMap) Allow(k DebounceKey, now time.Time) bool {
	if prev, ok := d.last.Load(k); ok {
		if now.Sub(prev.(time.Time)) < d.window {
			return false
		}
	}
	d.last.Store(k, now)
	return true
}

// LastSeen returns the most recent timestamp recorded for the key,
// or the zero time if the key has never been seen.
func (d *DebounceMap) LastSeen(k DebounceKey) time.Time {
	if prev, ok := d.last.Load(k); ok {
		return prev.(time.Time)
	}
	return time.Time{}
}
