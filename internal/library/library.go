// Package library indexes the read-only music directory and answers "which file is this
// song". The directory may hold several roots (one per mounted library); each is scanned
// and reported separately, so a folder the service may not read shows up as exactly that
// instead of as missing songs.
package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"trackferry/internal/audio"
	"trackferry/internal/names"
	"trackferry/internal/store"
)

// Playable formats by extension; anything else speakers cannot decode and is not indexed.
var formats = map[string]string{".mp3": "mp3", ".flac": "flac", ".m4a": "m4a", ".aac": "aac", ".ogg": "ogg", ".wav": "wav"}

// RootReport is one root's share of a scan.
type RootReport struct {
	Name       string   `json:"name"` // "" = files directly in the music directory
	Tracks     int      `json:"tracks"`
	ByName     int      `json:"by_name"`    // no tags: named after the file
	Unreadable int      `json:"unreadable"` // files or folders without permission
	Damaged    int      `json:"damaged"`    // unrecognised contents; indexed by name when possible
	Error      string   `json:"error,omitempty"`
	Samples    []string `json:"samples"` // a few unreadable or damaged paths
	done       bool
}

// Report describes a scan.
type Report struct {
	StartedMS  int64        `json:"started_ms"`
	FinishedMS int64        `json:"finished_ms,omitempty"`
	Error      string       `json:"error,omitempty"`
	Added      int          `json:"added"` // new or changed files read
	Removed    int64        `json:"removed"`
	Source     string       `json:"source,omitempty"`  // feiniu | files
	Total      int          `json:"total,omitempty"`   // songs 飞牛音乐 reports
	Skipped    int          `json:"skipped,omitempty"` // formats or CUE parts speakers cannot play
	Roots      []RootReport `json:"roots"`
}

// Status is the library as a whole.
type Status struct {
	Running  bool    `json:"running"`
	Seen     int     `json:"seen"` // files looked at by the running scan
	Tracks   int     `json:"tracks"`
	Artists  int     `json:"artists"`
	Last     *Report `json:"last"`
	Progress *Report `json:"progress,omitempty"`
}

// Library is safe for concurrent use.
type Library struct {
	dir string
	st  *store.Store
	log *slog.Logger
	// Remote, when linked, is the library; the music directory is then not scanned.
	Remote Remote

	mu      sync.RWMutex
	idx     *index
	loaded  bool // idx is what the database holds
	running bool
	seen    int
	cur     *Report
	last    *Report
	trigger chan struct{}
	retimed chan struct{}      // the interval was changed
	cancel  context.CancelFunc // stops the running scan
	loadAt  time.Time          // when a running scan last refreshed the index

	thumbs      string // where small covers are kept
	thumbWrites atomic.Int64
}

// New returns a Library over dir. Call Run to load the index and keep it fresh.
func New(dir string, st *store.Store, log *slog.Logger) *Library {
	return &Library{dir: dir, st: st, log: log, idx: newIndex(nil), trigger: make(chan struct{}, 1), retimed: make(chan struct{}, 1)}
}

// Load fills the in-memory index from the database.
func (l *Library) Load(ctx context.Context) error {
	rows, err := l.st.Tracks(ctx)
	if err != nil {
		return err
	}
	idx := newIndex(rows)
	l.mu.Lock()
	l.idx, l.loaded = idx, true
	l.mu.Unlock()
	return nil
}

// How often the library is read again, in minutes. 0: only when asked, and at the start.
const (
	settingEvery = "library.every_minutes"
	DefaultEvery = 6 * 60
)

// Minutes lists what may be set.
var Minutes = []int{0, 5, 15, 30, 60, 180, 360, 720, 1440}

// Every is how often the library is read again, in minutes.
func (l *Library) Every(ctx context.Context) int {
	v, _ := l.st.Setting(ctx, settingEvery)
	if n, err := strconv.Atoi(v); err == nil && slices.Contains(Minutes, n) {
		return n
	}
	return DefaultEvery
}

// SetEvery changes it.
func (l *Library) SetEvery(ctx context.Context, minutes int) error {
	if err := l.st.SetSetting(ctx, settingEvery, strconv.Itoa(minutes)); err != nil {
		return err
	}
	select {
	case l.retimed <- struct{}{}:
	default:
	}
	return nil
}

// until is the wait until the next scan; nil: none is due.
func (l *Library) until(ctx context.Context, since time.Time, retry time.Duration) <-chan time.Time {
	wait := retry
	if m := l.Every(ctx); m > 0 {
		every := time.Duration(m) * time.Minute
		if retry == 0 || every < retry {
			wait = every
		}
	}
	if wait == 0 {
		return nil
	}
	return time.After(time.Until(since.Add(wait)))
}

// Run loads the index, scans once, then again every interval or when asked.
func (l *Library) Run(ctx context.Context) {
	if raw, err := l.st.LastScan(ctx); err == nil && raw != "" {
		var r Report
		if json.Unmarshal([]byte(raw), &r) == nil {
			l.mu.Lock()
			l.last = &r
			l.mu.Unlock()
		}
	}
	if err := l.Load(ctx); err != nil {
		l.log.Warn("曲库　索引读不出来", "原因", err)
	}
	failed := 0
	for {
		sctx, cancel := context.WithCancel(ctx)
		l.mu.Lock()
		l.cancel = cancel
		l.mu.Unlock()
		retry := time.Duration(0)
		if err := l.guarded(sctx); err != nil && sctx.Err() == nil {
			l.log.Warn("曲库　同步失败", "原因", err)
			// 飞牛 may simply not be up yet: it starts along with us.
			retry = retryAfter << min(failed, 6)
			failed++
		} else {
			failed = 0
		}
		cancel()
		for since, due := time.Now(), false; !due; {
			select {
			case <-ctx.Done():
				return
			case <-l.until(ctx, since, retry):
				due = true
			case <-l.trigger:
				due = true
			case <-l.retimed:
			}
		}
	}
}

// retryAfter is the wait after a first failed scan; it doubles with every further one.
const retryAfter = time.Minute

// guarded is Scan, except that a fault in it is an error.
func (l *Library) guarded(ctx context.Context) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("程序出错：%v", p)
			l.log.Error("曲库　程序出错", "原因", fmt.Sprint(p), "位置", string(debug.Stack()))
		}
	}()
	return l.Scan(ctx)
}

// Rescan asks Run to scan now. It reports false when a scan is already running.
func (l *Library) Rescan() bool {
	l.mu.RLock()
	running := l.running
	l.mu.RUnlock()
	if running {
		return false
	}
	select {
	case l.trigger <- struct{}{}:
	default:
	}
	return true
}

// Restart drops the running scan and starts over (the library's source changed).
func (l *Library) Restart() {
	l.mu.RLock()
	cancel := l.cancel
	l.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	select {
	case l.trigger <- struct{}{}:
	default:
	}
}

// Status reports counts and the last or running scan.
func (l *Library) Status() Status {
	l.mu.RLock()
	defer l.mu.RUnlock()
	st := Status{Running: l.running, Seen: l.seen, Tracks: len(l.idx.rows), Artists: len(l.idx.byArtist), Last: l.last}
	if l.cur != nil {
		c := *l.cur
		c.Roots = append([]RootReport{}, l.cur.Roots...)
		st.Progress = &c
	}
	return st
}

// Size is the number of indexed tracks.
func (l *Library) Size() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.idx.rows)
}

const batch = 200

// Scan walks the music directory once. Unchanged files (same size and time) are not reopened.
func (l *Library) Scan(ctx context.Context) (err error) {
	started := time.Now()
	id, err := l.st.StartScan(ctx, started)
	if err != nil {
		return err
	}
	l.mu.RLock()
	loaded := l.loaded
	l.mu.RUnlock()
	if !loaded { // a scan cut short wrote what is not in memory yet
		if err := l.Load(ctx); err != nil {
			return err
		}
	}
	rep := &Report{StartedMS: started.UnixMilli(), Source: "files", Roots: []RootReport{}}
	l.mu.Lock()
	l.running, l.seen, l.cur = true, 0, rep
	known := make(map[string]store.TrackRow, len(l.idx.rows))
	for _, t := range l.idx.rows {
		known[t.Path] = t
	}
	l.mu.Unlock()
	defer func() {
		bg := context.WithoutCancel(ctx)
		state := "done"
		l.mu.Lock()
		rep.FinishedMS = time.Now().UnixMilli()
		if err != nil {
			state, rep.Error = "failed", err.Error()
			l.loaded = false
		}
		raw, _ := json.Marshal(rep)
		l.running, l.cur, l.last = false, nil, rep
		l.mu.Unlock()
		if e := l.st.FinishScan(bg, id, state, string(raw), time.Now()); e != nil {
			l.log.Warn("曲库　同步结果没有存上", "原因", e)
		}
		l.log.Info("曲库　同步结束", "状态", state, "用时ms", time.Since(started).Milliseconds(), "新增", rep.Added, "移除", rep.Removed, "共", l.Size())
	}()

	if l.Remote != nil && l.Remote.Linked() {
		return l.sync(ctx, id, rep, known)
	}
	if err := l.st.DropRemote(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) { // the folder is optional
		return err
	}
	format, _ := l.st.Setting(ctx, settingFilesFormat)
	reread := format != filesFormat
	roots := []string{}
	loose := false
	for _, e := range entries {
		if skip(e.Name()) {
			continue
		}
		if e.IsDir() {
			roots = append(roots, e.Name())
		} else if formats[strings.ToLower(filepath.Ext(e.Name()))] != "" {
			loose = true
		}
	}
	sort.Strings(roots)
	if loose {
		roots = append([]string{""}, roots...)
	}
	l.mu.Lock()
	for _, r := range roots {
		rep.Roots = append(rep.Roots, RootReport{Name: r, Samples: []string{}})
	}
	l.mu.Unlock()
	for i := range roots {
		if err := l.scanRoot(ctx, id, rep, &rep.Roots[i], known, reread); err != nil {
			return err
		}
	}
	for i := range rep.Roots {
		// Only a fully read root may lose tracks.
		if rr := &rep.Roots[i]; rr.done && rr.Unreadable == 0 && rr.Error == "" {
			n, err := l.st.DropUnseen(ctx, id, rr.Name)
			if err != nil {
				return err
			}
			l.mu.Lock()
			rep.Removed += n
			l.mu.Unlock()
		}
	}
	// A read of the folder that succeeded but found nothing still means what it said: the
	// music is gone. Only a folder that is not there at all (a mount that is down) keeps the
	// old tracks, so that they come back with the mount.
	if err == nil {
		if err := l.st.DropRootsExcept(ctx, roots); err != nil {
			return err
		}
	}
	if reread {
		whole := true
		for _, rr := range rep.Roots {
			// A folder that stays unreadable must not make every scan read the whole library
			// again: what could be read was, and anything unreadable is read when it becomes new.
			whole = whole && rr.done && rr.Error == ""
		}
		if whole {
			if err := l.st.SetSetting(ctx, settingFilesFormat, filesFormat); err != nil {
				return err
			}
		}
	}
	return l.Load(ctx)
}

func skip(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#")
}

func (l *Library) scanRoot(ctx context.Context, id int64, rep *Report, rr *RootReport, known map[string]store.TrackRow, reread bool) error {
	covers := &folderCovers{dir: l.dir, known: map[string]string{}}
	var changed []store.TrackRow
	var seen []int64
	flush := func() error {
		if len(changed)+len(seen) == 0 {
			return nil
		}
		err := l.st.SaveTracks(ctx, id, changed, seen)
		changed, seen = changed[:0], seen[:0]
		return err
	}
	note := func(p string, denied bool) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if denied {
			rr.Unreadable++
		} else {
			rr.Damaged++
		}
		if len(rr.Samples) < 5 {
			rr.Samples = append(rr.Samples, p)
		}
	}
	top := filepath.Join(l.dir, rr.Name)
	err := filepath.WalkDir(top, func(p string, d fs.DirEntry, werr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, _ := filepath.Rel(l.dir, p)
		rel = filepath.ToSlash(rel)
		if werr != nil {
			if p == top {
				l.mu.Lock()
				rr.Error = explain(werr)
				l.mu.Unlock()
			}
			note(rel, true)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p != top && skip(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if rr.Name == "" && p != top {
				return fs.SkipDir // other roots are scanned on their own
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		format := formats[ext]
		if format == "" || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			note(rel, true)
			return nil
		}
		l.mu.Lock()
		l.seen++
		l.mu.Unlock()
		old, ok := known[rel]
		if ok && !reread && old.Size == fi.Size() && old.MTimeMS == fi.ModTime().UnixMilli() {
			// Permissions change without touching size or time; a file we cannot open
			// must not stay a candidate.
			f, err := os.Open(p)
			if err != nil {
				note(rel, true)
				return nil
			}
			f.Close()
			// A folder image may have come or gone; a song's own picture stays with it.
			// A cover found online stays until the folder has one of its own.
			if c := covers.of(old.Path); old.Cover != localCoverID(old.Path) && c != old.Cover && !(c == "" && strings.HasPrefix(old.Cover, NetCover)) {
				old.Cover = c
				changed = append(changed, old)
			} else {
				seen = append(seen, old.ID)
			}
			l.count(rr, old.Tagged)
		} else {
			m, err := audio.ReadFile(p, ext)
			if err != nil && (errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist)) {
				note(rel, true)
				return nil
			}
			if err != nil {
				note(rel, false)
			}
			t := store.TrackRow{Path: rel, Root: rr.Name, Size: fi.Size(), MTimeMS: fi.ModTime().UnixMilli(), Title: m.Title, Artist: m.Artist,
				Album: m.Album, DurationMS: m.Duration.Milliseconds(), Format: format, SampleRate: m.SampleRate, Bits: m.Bits, Tagged: m.Title != ""}
			t.AlbumID = localAlbumID(rr.Name, rel, m.Album, m.AlbumArtist)
			t.Cover = covers.cover(t, m.Picture)
			if t.Cover == "" && ok && strings.HasPrefix(old.Cover, NetCover) {
				t.Cover = old.Cover
			}
			// When it came: kept across rereads; a new file counts from its own time, never later than now.
			t.AddedMS = min(fi.ModTime().UnixMilli(), time.Now().UnixMilli())
			if ok {
				t.Favorite, t.GUID = old.Favorite, old.GUID
				if old.AddedMS > 0 {
					t.AddedMS = old.AddedMS
				}
			}
			if t.Title == "" {
				var a string
				t.Title, a = audio.FromName(rel)
				if t.Artist == "" {
					t.Artist = a
				}
			} else if t.Artist == "" {
				if title, a := audio.FromName(rel); names.Norm(title) == names.Norm(t.Title) {
					t.Artist = a
				}
			}
			if t.Title == "" {
				t.Title = path.Base(rel)
			}
			changed = append(changed, t)
			l.count(rr, t.Tagged)
			l.mu.Lock()
			rep.Added++
			l.mu.Unlock()
		}
		if len(changed)+len(seen) >= batch {
			if err := flush(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	l.mu.Lock()
	rr.done = true
	l.mu.Unlock()
	// A finished root becomes searchable, but a library of many roots is not reloaded after
	// each one: the whole index is rebuilt at most every half minute, and at the end.
	if time.Since(l.loadAt) < 30*time.Second {
		return nil
	}
	l.loadAt = time.Now()
	return l.Load(ctx)
}

func (l *Library) count(rr *RootReport, tagged bool) {
	l.mu.Lock()
	rr.Tracks++
	if !tagged {
		rr.ByName++
	}
	l.mu.Unlock()
}

func explain(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return "没有读取权限"
	}
	return err.Error()
}
