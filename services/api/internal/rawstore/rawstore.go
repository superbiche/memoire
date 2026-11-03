// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package rawstore is raw/ custody with a cold layer: flat envelope files of
// closed UTC days are packed into zstd-compressed tar PARTS
// (raw/pack-YYYY-MM-DD.NNN.tar.zst) that REPLACE the flat files
// (liaison:D97 decision 2; records/history/L0006). Every reader of raw/ goes
// through Walk, which yields the same names, bytes and mtimes in the same
// global name order whether a file is flat or packed.
//
// Invariants:
//   - Parts are IMMUTABLE once published. The packer never rewrites a part:
//     a late file landing in an already-packed day goes into a NEW part of
//     that day. A by-name custody consumer (the snapshot) therefore never
//     sees a name whose content changed. The only rewrite is purge dropping
//     members, and every purge bumps the custody generation, which forces
//     the snapshot to re-baseline.
//   - A part holds members sorted by name, each name once in the store; all
//     members of a part belong to the part's UTC day. Walk merges a day's
//     parts and the flat files in name order and refuses a store that breaks
//     the order or holds a name twice with different bytes.
//   - A name may exist both flat and packed only in the crash window of the
//     packer (part published, flat copies not yet deleted). The bytes are
//     equal by construction; Walk yields the name once.
//   - A part is written to a temp file in TempDir (beside raw/, never inside
//     it, so the snapshot never captures one), verified by re-reading (names
//     and sha256 of every member), fsynced, then renamed into raw/. A temp
//     file is never custody: the flat originals are deleted only after the
//     rename. RemoveStaleTemps discards leftovers under the lock.
//   - Writers (packer, purge) and replaying readers take Lock, an flock on a
//     file next to raw/. The packer also refuses while a purge intent journal
//     exists, so it never relocates a file between a purge crash and resume.
package rawstore

import (
	"archive/tar"
	"bytes"
	"container/heap"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	packPrefix = "pack-"
	packSuffix = ".tar.zst"
	dayLayout  = "2006-01-02"
	maxSeq     = 999

	// DefaultMaxFilesPerPart bounds one part (and so one packer write and
	// one purge rewrite) independently of how many files a day received.
	DefaultMaxFilesPerPart = 200_000
)

// PartName is the name of part seq of the UTC day containing t.
func PartName(t time.Time, seq int) string {
	return fmt.Sprintf("%s%s.%03d%s", packPrefix, t.UTC().Format(dayLayout), seq, packSuffix)
}

// ParsePart returns the day and sequence of a part name.
func ParsePart(name string) (day time.Time, seq int, ok bool) {
	if !strings.HasPrefix(name, packPrefix) || !strings.HasSuffix(name, packSuffix) {
		return time.Time{}, 0, false
	}
	core := strings.TrimSuffix(strings.TrimPrefix(name, packPrefix), packSuffix)
	dayPart, seqPart, found := strings.Cut(core, ".")
	if !found || len(seqPart) != 3 {
		return time.Time{}, 0, false
	}
	d, err := time.Parse(dayLayout, dayPart)
	if err != nil {
		return time.Time{}, 0, false
	}
	n, err := strconv.Atoi(seqPart)
	if err != nil || n < 0 {
		return time.Time{}, 0, false
	}
	return d, n, true
}

// IsPack reports whether a raw/ entry name is a day-pack part.
func IsPack(name string) bool {
	_, _, ok := ParsePart(name)
	return ok
}

// IsEnvelope reports whether a raw/ entry name is an envelope file — the
// filter Replay and purge applied to the flat directory.
func IsEnvelope(name string) bool { return strings.HasSuffix(name, ".json") }

// DayOf returns the UTC day of a drainer-named raw file (<ms>-<rest>). Names
// without a millisecond prefix (hand-made, legacy) have no day and are
// never packed.
func DayOf(name string) (time.Time, bool) {
	prefix, _, ok := strings.Cut(name, "-")
	// Exactly 13 digits (2001-09-09 .. 2286-11-20): within one width,
	// lexical name order IS chronological order, which Walk relies on to
	// merge day parts with flat files. Other widths are never packed.
	if !ok || len(prefix) != 13 {
		return time.Time{}, false
	}
	// Digits only (review N4): ParseInt alone would accept a sign, and a
	// signed 12-digit name sorts out of chronological order.
	for i := 0; i < len(prefix); i++ {
		if prefix[i] < '0' || prefix[i] > '9' {
			return time.Time{}, false
		}
	}
	ms, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, false
	}
	t := time.UnixMilli(ms).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), true
}

func sideDir(rawDir, suffix string) string {
	clean := filepath.Clean(rawDir)
	return filepath.Join(filepath.Dir(clean), "."+filepath.Base(clean)+suffix)
}

// TempDir holds part temp files: beside raw/ on the same filesystem (rename
// stays atomic), outside the snapshot's raw/ inventory.
func TempDir(rawDir string) string { return sideDir(rawDir, "-pack-tmp") }

// IncomingDir is where the drainer stages a raw copy before publishing it
// into raw/ with a link, so raw/ never shows a partially written file.
func IncomingDir(rawDir string) string { return sideDir(rawDir, "-incoming") }

// RemoveStaleTemps deletes every file in TempDir and IncomingDir. Call it
// under Lock with the drainer quiesced (purge's precondition) — or, for the
// packer, TempDir only via removeStalePackTemps. A temp is never custody:
// pack temps hold copies of flat files that still exist, incoming temps hold
// copies of queue items that are still in the queue.
func RemoveStaleTemps(rawDir string) (int, error) {
	n, err := removeAll(TempDir(rawDir))
	if err != nil {
		return n, err
	}
	m, err := removeAll(IncomingDir(rawDir))
	return n + m, err
}

func removeAll(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return n, err
		}
		n++
	}
	return n, nil
}

// Entry locates one raw file. Pack is empty for a flat file. AlsoFlat marks
// a packed name whose flat copy still exists (crash window): destroyers must
// remove both.
type Entry struct {
	Name     string
	ModTime  time.Time
	Pack     string
	AlsoFlat bool
}

// Member is one part member, fully read.
type Member struct {
	Name    string
	ModTime time.Time
	Body    []byte
}

// partReader streams one part's members, refusing non-ascending order.
type partReader struct {
	name string
	day  time.Time // zero = no day check
	f    *os.File
	zr   *zstd.Decoder
	tr   *tar.Reader
	prev string
	cur  Member
	done bool
}

func openPart(path string) (*partReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	zr, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("pack %s: %w", filepath.Base(path), err)
	}
	p := &partReader{name: filepath.Base(path), f: f, zr: zr, tr: tar.NewReader(zr)}
	if day, _, ok := ParsePart(p.name); ok {
		p.day = day
	}
	return p, nil
}

func (p *partReader) close() {
	p.zr.Close()
	_ = p.f.Close()
}

// next advances to the next member; done is set at the end.
func (p *partReader) next() error {
	hdr, err := p.tr.Next()
	if errors.Is(err, io.EOF) {
		p.done = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("pack %s: %w", p.name, err)
	}
	if hdr.Typeflag != tar.TypeReg || strings.Contains(hdr.Name, "/") {
		return fmt.Errorf("pack %s: unexpected member %q", p.name, hdr.Name)
	}
	if hdr.Name <= p.prev {
		return fmt.Errorf("pack %s: member %q out of order after %q", p.name, hdr.Name, p.prev)
	}
	if !p.day.IsZero() {
		if d, ok := DayOf(hdr.Name); !ok || !d.Equal(p.day) {
			return fmt.Errorf("pack %s: member %q does not belong to the part's day", p.name, hdr.Name)
		}
	}
	p.prev = hdr.Name
	body, err := io.ReadAll(p.tr)
	if err != nil {
		return fmt.Errorf("pack %s: member %s: %w", p.name, hdr.Name, err)
	}
	p.cur = Member{Name: hdr.Name, ModTime: hdr.ModTime, Body: body}
	return nil
}

// ReadPack streams one part's members in stored (name) order without
// holding the part in memory.
func ReadPack(path string, fn func(Member) error) error {
	p, err := openPart(path)
	if err != nil {
		return err
	}
	defer p.close()
	for {
		if err := p.next(); err != nil {
			return err
		}
		if p.done {
			return nil
		}
		if err := fn(p.cur); err != nil {
			return err
		}
	}
}

// Members reads every member of one part (tests, small parts).
func Members(path string) ([]Member, error) {
	var out []Member
	err := ReadPack(path, func(m Member) error {
		out = append(out, m)
		return nil
	})
	return out, err
}

type listing struct {
	flats []string               // sorted names
	parts map[time.Time][]string // day -> part names, sorted
	days  []time.Time            // sorted
}

// list reads raw/ names only (no per-entry stat: a 3.5M-file directory
// stays a few hundred MB) and classifies them. Flat files are stat'ed when
// read; non-regular entries are skipped then.
func list(rawDir string, accept func(string) bool) (listing, error) {
	d, err := os.Open(rawDir)
	if err != nil {
		return listing{}, err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return listing{}, err
	}
	sort.Strings(names)
	l := listing{parts: map[time.Time][]string{}}
	for _, name := range names {
		if day, _, ok := ParsePart(name); ok {
			if _, seen := l.parts[day]; !seen {
				l.days = append(l.days, day)
			}
			l.parts[day] = append(l.parts[day], name)
			continue
		}
		if accept(name) {
			l.flats = append(l.flats, name)
		}
	}
	sort.Slice(l.days, func(i, j int) bool { return l.days[i].Before(l.days[j]) })
	return l, nil
}

// partHeap orders a day's open part readers by their current member name.
type partHeap []*partReader

func (h partHeap) Len() int           { return len(h) }
func (h partHeap) Less(i, j int) bool { return h[i].cur.Name < h[j].cur.Name }
func (h partHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *partHeap) Push(x any)        { *h = append(*h, x.(*partReader)) }
func (h *partHeap) Pop() any {
	old := *h
	p := old[len(old)-1]
	*h = old[:len(old)-1]
	return p
}

// Walk yields every envelope (*.json) in raw/ once, flat or packed, in
// global name order — the order the flat-directory readers used.
func Walk(rawDir string, fn func(Entry, []byte) error) error {
	return walk(rawDir, IsEnvelope, fn)
}

// WalkFiles is Walk over every regular non-part file (the retro-sweep's
// historical input set); part members are envelopes and included.
func WalkFiles(rawDir string, fn func(Entry, []byte) error) error {
	return walk(rawDir, func(string) bool { return true }, fn)
}

func walk(rawDir string, accept func(string) bool, fn func(Entry, []byte) error) error {
	l, err := list(rawDir, accept)
	if err != nil {
		return err
	}
	last := ""
	emit := func(e Entry, body []byte) error {
		if last != "" && e.Name <= last {
			return fmt.Errorf("raw custody out of order or duplicated: %q after %q", e.Name, last)
		}
		last = e.Name
		return fn(e, body)
	}
	fi := 0
	flushFlats := func(before string, all bool) error {
		for fi < len(l.flats) && (all || l.flats[fi] < before) {
			name := l.flats[fi]
			fi++
			info, err := os.Lstat(filepath.Join(rawDir, name))
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			body, err := os.ReadFile(filepath.Join(rawDir, name))
			if err != nil {
				return err
			}
			if err := emit(Entry{Name: name, ModTime: info.ModTime()}, body); err != nil {
				return err
			}
		}
		return nil
	}
	for _, day := range l.days {
		if err := walkDay(rawDir, l.parts[day], func(m Member, part string) error {
			if err := flushFlats(m.Name, false); err != nil {
				return err
			}
			e := Entry{Name: m.Name, ModTime: m.ModTime, Pack: part}
			if fi < len(l.flats) && l.flats[fi] == m.Name {
				flatBody, err := os.ReadFile(filepath.Join(rawDir, m.Name))
				if err != nil {
					return err
				}
				if !bytes.Equal(flatBody, m.Body) {
					return fmt.Errorf("raw custody conflict: %s differs between flat and %s", m.Name, part)
				}
				fi++
				e.AlsoFlat = true
			}
			return emit(e, m.Body)
		}); err != nil {
			return err
		}
	}
	return flushFlats("", true)
}

// walkDay merges one day's parts in member-name order.
func walkDay(rawDir string, parts []string, fn func(Member, string) error) error {
	h := partHeap{}
	defer func() {
		for _, p := range h {
			p.close()
		}
	}()
	for _, part := range parts {
		p, err := openPart(filepath.Join(rawDir, part))
		if err != nil {
			return err
		}
		if err := p.next(); err != nil {
			p.close()
			return err
		}
		if p.done {
			p.close()
			continue
		}
		h = append(h, p)
	}
	heap.Init(&h)
	for h.Len() > 0 {
		p := h[0]
		if err := fn(p.cur, p.name); err != nil {
			return err
		}
		if err := p.next(); err != nil {
			return err
		}
		if p.done {
			heap.Pop(&h)
			p.close()
			continue
		}
		heap.Fix(&h, 0)
	}
	return nil
}

type memberSum struct {
	name string
	sum  [sha256.Size]byte
}

// writeVerified streams members from next() into a part temp in TempDir,
// verifies it by re-reading, fsyncs and renames it to raw/<part>. With zero
// members nothing is published and the old part (if any) is removed.
func writeVerified(rawDir, part string, next func(put func(name string, mtime time.Time, body []byte) error) error) (int, error) {
	tmpDir := TempDir(rawDir)
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return 0, err
	}
	tmp := filepath.Join(tmpDir, part+".tmp")
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	cleanup := func() { _ = out.Close(); _ = os.Remove(tmp) }
	zw, err := zstd.NewWriter(out, zstd.WithEncoderConcurrency(1))
	if err != nil {
		cleanup()
		return 0, err
	}
	tw := tar.NewWriter(zw)
	var written []memberSum
	put := func(name string, mtime time.Time, body []byte) error {
		// Whole seconds, truncated like the flat readers' ModTime().Unix():
		// an unspecified tar format ROUNDS sub-second mtimes.
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), ModTime: mtime.Truncate(time.Second), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
		written = append(written, memberSum{name, sha256.Sum256(body)})
		return nil
	}
	if err := next(put); err != nil {
		cleanup()
		return 0, err
	}
	if err := tw.Close(); err != nil {
		cleanup()
		return 0, err
	}
	if err := zw.Close(); err != nil {
		cleanup()
		return 0, err
	}
	path := filepath.Join(rawDir, part)
	if len(written) == 0 {
		cleanup()
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		return 0, syncDir(rawDir)
	}
	if err := out.Sync(); err != nil {
		cleanup()
		return 0, err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := verify(tmp, written); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return len(written), syncDir(rawDir)
}

// verify re-reads a written part and checks it holds exactly the members
// written, in order, with identical bytes.
func verify(path string, want []memberSum) error {
	i := 0
	err := ReadPack(path, func(m Member) error {
		if i >= len(want) || want[i].name != m.Name || want[i].sum != sha256.Sum256(m.Body) {
			return fmt.Errorf("pack verify %s: member %d (%s) does not match what was written", filepath.Base(path), i, m.Name)
		}
		i++
		return nil
	})
	if err != nil {
		return err
	}
	if i != len(want) {
		return fmt.Errorf("pack verify %s: %d members read, %d written", filepath.Base(path), i, len(want))
	}
	return nil
}

// WritePart publishes a NEW part holding the given flat files of rawDir
// (sorted, read once, verified). It refuses an existing part name: parts are
// immutable. It never deletes the flat files; the caller does after a nil
// error.
func WritePart(rawDir, part string, names []string) (int, error) {
	if !IsPack(part) {
		return 0, fmt.Errorf("not a part name: %q", part)
	}
	if _, err := os.Stat(filepath.Join(rawDir, part)); err == nil {
		return 0, fmt.Errorf("part %s exists: parts are immutable", part)
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return 0, fmt.Errorf("duplicate name %q", sorted[i])
		}
	}
	if len(sorted) == 0 {
		return 0, errors.New("empty part")
	}
	return writeVerified(rawDir, part, func(put func(string, time.Time, []byte) error) error {
		for _, name := range sorted {
			p := filepath.Join(rawDir, name)
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			body, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				return err
			}
			if err := put(name, info.ModTime(), body); err != nil {
				return err
			}
		}
		return nil
	})
}

// DropResult reports one purge rewrite.
type DropResult struct {
	Members int  // members left in the part (0 = part removed)
	Dropped int  // members removed
	Written bool // false when nothing matched (no write happened)
}

// DropMembers rewrites a part without the named members — purge's lever,
// always followed by a custody generation bump. Idempotent: a repeat drops
// nothing and writes nothing. A part left empty is removed.
func DropMembers(rawDir, part string, drop map[string]bool) (DropResult, error) {
	var res DropResult
	if !IsPack(part) {
		return res, fmt.Errorf("not a part name: %q", part)
	}
	path := filepath.Join(rawDir, part)
	found := 0
	if err := ReadPack(path, func(m Member) error {
		if drop[m.Name] {
			found++
		}
		return nil
	}); err != nil {
		return res, err
	}
	if found == 0 {
		return res, nil
	}
	members, err := writeVerified(rawDir, part, func(put func(string, time.Time, []byte) error) error {
		return ReadPack(path, func(m Member) error {
			if drop[m.Name] {
				res.Dropped++
				return nil
			}
			return put(m.Name, m.ModTime, m.Body)
		})
	})
	if err != nil {
		return res, err
	}
	res.Members = members
	res.Written = true
	return res, nil
}

// Locate maps each name to the parts that hold it, searching the parts of
// the name's UTC day (undated names are never packed).
func Locate(rawDir string, names []string) (map[string][]string, error) {
	want := map[time.Time]map[string]bool{}
	for _, n := range names {
		if day, ok := DayOf(n); ok {
			if want[day] == nil {
				want[day] = map[string]bool{}
			}
			want[day][n] = true
		}
	}
	out := map[string][]string{}
	if len(want) == 0 {
		return out, nil
	}
	l, err := list(rawDir, func(string) bool { return false })
	if err != nil {
		return nil, err
	}
	for day, set := range want {
		for _, part := range l.parts[day] {
			if err := ReadPack(filepath.Join(rawDir, part), func(m Member) error {
				if set[m.Name] {
					out[m.Name] = append(out[m.Name], part)
				}
				return nil
			}); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// PackOptions bounds one packer run.
type PackOptions struct {
	Now             time.Time // reference clock (UTC day boundaries)
	GraceDays       int       // days before today that stay flat; days older are closed
	MaxParts        int       // 0 = no bound: stop after writing N parts
	MaxSeconds      int       // 0 = no bound; checked between parts
	MaxFilesPerPart int       // 0 = DefaultMaxFilesPerPart
	BlockingFiles   []string  // refuse while any exists (purge intent journals)
	Apply           bool      // false = report only
}

// PackReport is the packer's run report: counts and day names, never content.
type PackReport struct {
	DryRun             bool     `json:"dry_run"`
	Cutoff             string   `json:"cutoff_day"`
	DaysPacked         []string `json:"days"`
	PartsWritten       int      `json:"parts_written"`
	FlatFilesPacked    int      `json:"flat_files_packed"`
	FlatFilesDeleted   int      `json:"flat_files_deleted"`
	AlreadyPackedFlats int      `json:"already_packed_flats"`
	FlatBytesPacked    int64    `json:"flat_bytes_packed"`
	UndatedFiles       int      `json:"undated_files"`
	OpenDayFiles       int      `json:"open_day_files"`
	StaleTempRemoved   int      `json:"stale_temp_removed"`
	Stopped            bool     `json:"stopped"`
}

// PackClosedDays packs every flat envelope of a closed UTC day (older than
// Now's day minus GraceDays) into NEW parts of that day (at most
// MaxFilesPerPart members each), then deletes the flat files the part
// provably holds. A flat file already held by an existing part with the same
// bytes (crash window) is only deleted. Apply takes the raw lock; a dry run
// only counts.
func PackClosedDays(rawDir string, opts PackOptions) (PackReport, error) {
	var rep PackReport
	rep.DryRun = !opts.Apply
	if opts.GraceDays < 1 {
		return rep, errors.New("grace days must be at least 1 (the current day is never closed)")
	}
	perPart := opts.MaxFilesPerPart
	if perPart <= 0 {
		perPart = DefaultMaxFilesPerPart
	}
	now := opts.Now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	cutoff := today.AddDate(0, 0, -opts.GraceDays)
	rep.Cutoff = cutoff.Format(dayLayout)
	start := time.Now()

	// Custody contract item 1: lock, then the purge-intent interlock under
	// it; an apply also discards stale part temps.
	mode := ModeRead
	if opts.Apply {
		mode = ModeMove
	}
	sess, err := Custody{RawDir: rawDir, Intents: opts.BlockingFiles}.Enter(mode)
	if err != nil {
		return rep, err
	}
	defer sess.Release()
	rep.StaleTempRemoved = sess.StaleTempsRemoved

	l, err := list(rawDir, IsEnvelope)
	if err != nil {
		return rep, err
	}
	byDay := map[time.Time][]string{}
	var days []time.Time
	for _, e := range l.flats {
		day, ok := DayOf(e)
		switch {
		case !ok:
			rep.UndatedFiles++
		case !day.Before(cutoff):
			rep.OpenDayFiles++
		default:
			if _, seen := byDay[day]; !seen {
				days = append(days, day)
			}
			byDay[day] = append(byDay[day], e)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	stop := func() bool {
		return (opts.MaxParts > 0 && rep.PartsWritten >= opts.MaxParts) ||
			(opts.MaxSeconds > 0 && time.Since(start) > time.Duration(opts.MaxSeconds)*time.Second)
	}
	for _, day := range days {
		if stop() {
			rep.Stopped = true
			break
		}
		files := byDay[day]
		rep.DaysPacked = append(rep.DaysPacked, day.Format(dayLayout))
		for _, f := range files {
			if info, err := os.Lstat(filepath.Join(rawDir, f)); err == nil {
				rep.FlatBytesPacked += info.Size()
			}
		}
		rep.FlatFilesPacked += len(files)
		if !opts.Apply {
			continue
		}
		// Names the day's existing parts already hold: identical bytes =
		// crash-window copy (delete only); different bytes = conflict.
		existing := l.parts[day]
		var toPack []string
		if len(existing) > 0 {
			sums := map[string][sha256.Size]byte{}
			for _, part := range existing {
				if err := ReadPack(filepath.Join(rawDir, part), func(m Member) error {
					sums[m.Name] = sha256.Sum256(m.Body)
					return nil
				}); err != nil {
					return rep, err
				}
			}
			for _, f := range files {
				sum, held := sums[f]
				if !held {
					toPack = append(toPack, f)
					continue
				}
				body, err := os.ReadFile(filepath.Join(rawDir, f))
				if err != nil {
					return rep, err
				}
				if sha256.Sum256(body) != sum {
					return rep, fmt.Errorf("raw custody conflict: %s differs between flat and an existing part of %s", f, day.Format(dayLayout))
				}
				if err := os.Remove(filepath.Join(rawDir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return rep, err
				}
				rep.FlatFilesDeleted++
				rep.AlreadyPackedFlats++
			}
		} else {
			toPack = files
		}
		seq := nextSeq(existing)
		for i := 0; i < len(toPack); i += perPart {
			if i > 0 && stop() {
				rep.Stopped = true
				return rep, syncDir(rawDir)
			}
			if seq > maxSeq {
				return rep, fmt.Errorf("day %s has no free part sequence left", day.Format(dayLayout))
			}
			chunk := toPack[i:min(i+perPart, len(toPack))]
			if _, err := WritePart(rawDir, PartName(day, seq), chunk); err != nil {
				return rep, fmt.Errorf("pack %s: %w", day.Format(dayLayout), err)
			}
			seq++
			rep.PartsWritten++
			for _, name := range chunk {
				if err := os.Remove(filepath.Join(rawDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return rep, fmt.Errorf("delete packed flat file %s: %w", name, err)
				}
				rep.FlatFilesDeleted++
			}
			if err := syncDir(rawDir); err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

func checkBlocking(files []string) error {
	for _, f := range files {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			return fmt.Errorf("refusing: %s exists (a purge is unfinished; resume it first — moving raw files now could take a journaled file out of its reach)", f)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// UnpackReport is the rollback report: counts and names, never content.
type UnpackReport struct {
	Part        string `json:"part"`
	Generation  int64  `json:"generation"`
	Restored    int    `json:"flat_files_restored"`
	AlreadyFlat int    `json:"already_flat"`
	PartRemoved bool   `json:"part_removed"`
}

// Unpack is the rollback of one part: under the custody lock (and refusing
// while a purge intent exists) it FIRST advances the custody generation in
// manifestPath, then restores every member as a flat file (staged in
// IncomingDir, linked into raw/, original mtime), then removes the part.
// The generation bump makes by-name custody consumers (the snapshot)
// re-baseline, so the part name may be reused later without a consumer
// believing it already holds the new content (review N1). Crash-safe by
// re-running: a second bump is harmless, members already flat with the same
// bytes are skipped, and a flat copy with different bytes is a conflict.
func Unpack(rawDir, part, manifestPath string, blocking []string) (UnpackReport, error) {
	rep := UnpackReport{Part: part}
	if !IsPack(part) {
		return rep, fmt.Errorf("not a part name: %q", part)
	}
	path := filepath.Join(rawDir, part)
	c := Custody{RawDir: rawDir, ManifestPath: manifestPath, Intents: blocking}
	err := c.Rename("raw-unpack "+part, func() error {
		_, err := os.Stat(path)
		return err
	}, func(gen int64) error {
		rep.Generation = gen
		stage := IncomingDir(rawDir)
		if err := os.MkdirAll(stage, 0o700); err != nil {
			return err
		}
		err := ReadPack(path, func(m Member) error {
			dst := filepath.Join(rawDir, m.Name)
			if existing, err := os.ReadFile(dst); err == nil {
				if !bytes.Equal(existing, m.Body) {
					return fmt.Errorf("raw custody conflict: flat %s differs from %s", m.Name, part)
				}
				rep.AlreadyFlat++
				return nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			tmp, err := os.CreateTemp(stage, m.Name+".*.tmp")
			if err != nil {
				return err
			}
			tmpName := tmp.Name()
			defer os.Remove(tmpName)
			if _, err := tmp.Write(m.Body); err != nil {
				tmp.Close()
				return err
			}
			if err := tmp.Sync(); err != nil {
				tmp.Close()
				return err
			}
			if err := tmp.Close(); err != nil {
				return err
			}
			if err := os.Chtimes(tmpName, m.ModTime, m.ModTime); err != nil {
				return err
			}
			if err := os.Link(tmpName, dst); err != nil {
				return err
			}
			rep.Restored++
			return nil
		})
		if err != nil {
			return err
		}
		if err := syncDir(rawDir); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		rep.PartRemoved = true
		return syncDir(rawDir)
	})
	return rep, err
}

// BumpGeneration advances the custody generation in the custody manifest
// (custody/MANIFEST.json, shared with memoire-purge and read by the
// snapshot): generation+1, updated_at_epoch, and a last_raw_change note.
// Other keys are preserved. A missing manifest starts at generation 0; a
// corrupt one refuses (mirror lineage must never reset).
func BumpGeneration(manifestPath, why string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		return 0, err
	}
	m := map[string]any{}
	var gen int64
	b, err := os.ReadFile(manifestPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return 0, err
	default:
		// An existing manifest must be exactly one JSON object carrying a
		// non-negative integer generation (review N7). Only an absent file
		// starts at generation 0: mirror lineage must never reset.
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var raw any
		if err := dec.Decode(&raw); err != nil {
			return 0, fmt.Errorf("corrupt custody manifest %s: %w", manifestPath, err)
		}
		if _, err := dec.Token(); err != io.EOF {
			return 0, fmt.Errorf("corrupt custody manifest %s: trailing data", manifestPath)
		}
		obj, isObj := raw.(map[string]any)
		if !isObj || obj == nil {
			return 0, fmt.Errorf("corrupt custody manifest %s: not a JSON object", manifestPath)
		}
		m = obj
		v, has := m["generation"]
		if !has {
			return 0, fmt.Errorf("corrupt custody manifest %s: no generation", manifestPath)
		}
		n, isNum := v.(json.Number)
		if !isNum {
			return 0, fmt.Errorf("corrupt custody manifest %s: non-numeric generation", manifestPath)
		}
		g, err := n.Int64()
		if err != nil || g < 0 || g == math.MaxInt64 {
			return 0, fmt.Errorf("corrupt custody manifest %s: generation %q is not a usable non-negative integer", manifestPath, n.String())
		}
		gen = g
	}
	gen++
	now := time.Now().Unix()
	m["generation"] = gen
	m["updated_at_epoch"] = now
	m["last_raw_change"] = map[string]any{"at_epoch": now, "change": why}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return 0, err
	}
	tmp := manifestPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(append(out, '\n')); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, manifestPath); err != nil {
		return 0, err
	}
	return gen, syncDir(filepath.Dir(manifestPath))
}

func nextSeq(parts []string) int {
	next := 0
	for _, p := range parts {
		if _, seq, ok := ParsePart(p); ok && seq >= next {
			next = seq + 1
		}
	}
	return next
}

// LockPath is the raw/ custody lock: a dotfile beside raw/, never inside it
// (the snapshot captures every top-level raw/ file by name).
func LockPath(rawDir string) string { return sideDir(rawDir, ".lock") }

// Lock takes the exclusive, non-blocking raw/ custody lock shared by the
// packer, purge, replay, the sweep and the snapshot script (`flock` on
// LockPath).
func Lock(rawDir string) (func(), error) {
	f, err := os.OpenFile(LockPath(rawDir), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("raw custody lock %s is held (packer, purge, replay, sweep or snapshot running): %w", LockPath(rawDir), err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
