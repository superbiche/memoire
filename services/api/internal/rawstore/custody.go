// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package rawstore

import (
	"errors"
	"path/filepath"
)

// Custody is the single entry point for every raw/ operation beyond the
// drainer's single-file publish (operator ruling a76; restructure contract
// in the review chain 2026-10-11-memoire-raw-cold-archive):
//
//  1. Every multi-file read (Replay, retro-sweep, --verify, purge scan) and
//     every mutation (packer, purge, unpack) runs inside the custody lock,
//     and the purge-intent interlock is evaluated UNDER that lock.
//  2. Every operation that can remove a part name advances the custody
//     generation BEFORE the name disappears (Rename). A part name is reused
//     only in a later generation, so by-name consumers (the snapshot)
//     re-baseline first. Purge keeps its own journal: its generation bump
//     follows the deletion, but its intent journal blocks every other
//     Custody user until that bump has happened.
//  3. The drainer's publish stays lock-free: an atomic link of a complete
//     file. Packing never takes open-day names, and late names land in a
//     new part.
type Custody struct {
	RawDir       string
	ManifestPath string   // custody/MANIFEST.json; required by Rename
	Intents      []string // purge intent journals; nil = DefaultIntentPath(RawDir)
	SkipIntents  bool     // purge only: it owns the intent journal itself
}

// Mode selects what Enter prepares.
type Mode int

const (
	// ModeRead: lock + intent interlock.
	ModeRead Mode = iota
	// ModeMove: ModeRead + discard stale part temps (never custody).
	ModeMove
)

// beforeIntentCheck is a test seam: it runs after the lock is held and
// before the intent interlock is evaluated (contract item 1, oracle C3).
var beforeIntentCheck = func() {}

// DefaultIntentPath is memoire-purge's intent journal for a raw/ beside it
// (<home>/purge-intent.json for <home>/raw).
func DefaultIntentPath(rawDir string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(rawDir)), "purge-intent.json")
}

func (c Custody) intents() []string {
	switch {
	case c.SkipIntents:
		return nil
	case c.Intents != nil:
		return c.Intents
	default:
		return []string{DefaultIntentPath(c.RawDir)}
	}
}

// Session is a held custody lock.
type Session struct {
	c                 Custody
	release           func()
	StaleTempsRemoved int
}

// Release drops the custody lock. Safe to call more than once.
func (s *Session) Release() {
	if s.release != nil {
		s.release()
		s.release = nil
	}
}

// BumpGeneration advances the custody generation while the lock is held.
func (s *Session) BumpGeneration(why string) (int64, error) {
	if s.c.ManifestPath == "" {
		return 0, errors.New("custody manifest path not set")
	}
	return BumpGeneration(s.c.ManifestPath, why)
}

// Enter takes the custody lock (non-blocking), then evaluates the intent
// interlock under it, then prepares the mode.
func (c Custody) Enter(mode Mode) (*Session, error) {
	unlock, err := Lock(c.RawDir)
	if err != nil {
		return nil, err
	}
	s := &Session{c: c, release: unlock}
	beforeIntentCheck()
	if err := checkBlocking(c.intents()); err != nil {
		s.Release()
		return nil, err
	}
	if mode == ModeMove {
		n, err := removeAll(TempDir(c.RawDir))
		if err != nil {
			s.Release()
			return nil, err
		}
		s.StaleTempsRemoved = n
	}
	return s, nil
}

// Read runs fn under ModeRead.
func (c Custody) Read(fn func() error) error {
	s, err := c.Enter(ModeRead)
	if err != nil {
		return err
	}
	defer s.Release()
	return fn()
}

// Move runs fn under ModeMove.
func (c Custody) Move(fn func(*Session) error) error {
	s, err := c.Enter(ModeMove)
	if err != nil {
		return err
	}
	defer s.Release()
	return fn(s)
}

// Rename runs fn under ModeMove after precheck succeeds and AFTER the
// custody generation has advanced: the only way to remove a part name
// outside purge (contract item 2).
func (c Custody) Rename(why string, precheck func() error, fn func(gen int64) error) error {
	return c.Move(func(s *Session) error {
		if precheck != nil {
			if err := precheck(); err != nil {
				return err
			}
		}
		gen, err := s.BumpGeneration(why)
		if err != nil {
			return err
		}
		return fn(gen)
	})
}
