// Package cache keeps the online songs that were played, so that asking for one again needs
// no platform. Songs stay until the limit is reached; then those unused longest make room.
package cache

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mp3 "trackferry/internal/audio"
	"trackferry/internal/online"
)

// Limits. The limit is in MB; 0 switches the cache off, NoLimit lets it grow.
const (
	NoLimit   = -1
	DefaultMB = 1024
	MaxMB     = 100 << 10
	maxSong   = 200 << 20
	minSong   = 100 << 10
	keepFree  = 1 << 30 // left on the disk for everything else
	maxAsked  = 20
)

// Valid reports whether mb can be a limit.
func Valid(mb int) bool { return mb >= NoLimit && mb <= MaxMB }

func toBytes(mb int) int64 {
	if mb == NoLimit {
		return math.MaxInt64
	}
	return int64(mb) << 20
}

// Opener opens a song's audio at its platform.
type Opener = func(ctx context.Context, rng string) (*http.Response, error)

type entry struct {
	Song   online.Saved `json:"song"`
	Asked  []string     `json:"asked"` // requests this song answered
	File   string       `json:"file"`  // its name; before 1.2.1 it was the key
	Size   int64        `json:"size"`
	UsedMS int64        `json:"used_ms"`
}

// Status is what the settings page shows.
type Status struct {
	Mounted bool  `json:"mounted"` // on a disk of its own
	LimitMB int   `json:"limit_mb"`
	Bytes   int64 `json:"bytes"`
	Songs   int   `json:"songs"`
}

// Cache is safe for concurrent use.
type Cache struct {
	dir   string // the index, kept apart from the audio: a sleeping disk must not slow a request
	audio string
	read  func(key, rng string) (*http.Response, error)
	log   *slog.Logger
	free  func(dir string) (int64, error)

	one sync.Mutex // one download at a time

	mu    sync.Mutex
	limit int64
	songs map[string]*entry
	gen   int // raised by Clear: what is being downloaded is no longer wanted
}

// Open reads the cache in dir. The audio is in there too, or in mount if there is one; then
// what dir still holds from before is removed.
func Open(dir, mount string, limitMB int, log *slog.Logger) (*Cache, error) {
	c := &Cache{dir: dir, audio: dir, log: log, free: free, limit: toBytes(limitMB), songs: map[string]*entry{}}
	c.read = c.serve
	if mount != "" {
		c.audio = mount
	}
	for _, d := range []string{dir, c.audio} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if c.mounted() { // only then is the other place given up
		f, err := os.CreateTemp(c.audio, "*.part")
		if err != nil {
			return nil, err
		}
		f.Close()
		os.Remove(f.Name())
		c.leave(dir)
	}
	if raw, err := os.ReadFile(c.index()); err == nil {
		if json.Unmarshal(raw, &c.songs) != nil || c.songs == nil {
			c.songs = map[string]*entry{}
		}
	}
	for k, e := range c.songs {
		if e != nil && e.File != "" && !ownName(e.File) {
			delete(c.songs, k)
			continue
		}
		fi, err := os.Stat(c.file(k))
		if e == nil || len(k) != 64 || err != nil || fi.Size() != e.Size {
			delete(c.songs, k)
		}
	}
	c.sweep(c.audio) // what a crash left behind
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rename()
	c.trim(0)
	c.save()
	return c, nil
}

func (c *Cache) mounted() bool { return c.audio != c.dir }

// file is where a song is. The caller holds the lock.
func (c *Cache) file(key string) string {
	name := key + ".mp3"
	if e := c.songs[key]; e != nil && e.File != "" {
		name = e.File
	}
	return filepath.Join(c.audio, name)
}

func (c *Cache) taken(name string) bool {
	for _, e := range c.songs {
		if strings.EqualFold(e.File, name) {
			return true
		}
	}
	return false
}

// rename gives songs kept before 1.2.1 their names. The index goes first: a file it does not
// name under its old key is swept, one it names wrongly is fetched again.
func (c *Cache) rename() {
	old := map[string]string{}
	for k, e := range c.songs {
		if e.File == "" {
			old[k] = c.file(k)
			e.File = freeName(c.audio, e.Song, e.Size, c.taken)
		}
	}
	if len(old) == 0 {
		return
	}
	c.save()
	for k, from := range old {
		if os.Rename(from, c.file(k)) != nil {
			os.Remove(from)
			delete(c.songs, k)
		}
	}
}

// leave removes what dir holds from before the mount.
func (c *Cache) leave(dir string) {
	index := filepath.Join(dir, "index.json")
	var songs map[string]*entry
	if raw, err := os.ReadFile(index); err == nil && json.Unmarshal(raw, &songs) == nil {
		for _, e := range songs {
			if e != nil && ownName(e.File) {
				os.Remove(filepath.Join(dir, e.File))
			}
		}
	}
	c.sweep(dir)
	os.Remove(index)
}

// index is another file for a mount, so that one start without it forgets nothing.
func (c *Cache) index() string {
	if c.mounted() {
		return filepath.Join(c.dir, "mounted.json")
	}
	return filepath.Join(c.dir, "index.json")
}

// sweep removes half downloads, and audio under a key that no song of ours is in. Anything else there stays.
func (c *Cache) sweep(dir string) {
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		k, song := strings.CutSuffix(f.Name(), ".mp3")
		if song && len(k) == 64 && c.songs[k] == nil || strings.HasSuffix(f.Name(), ".part") {
			os.Remove(filepath.Join(dir, f.Name()))
		}
	}
}

func keyOf(s online.Saved) string {
	sum := sha256.Sum256([]byte(s.Source + "\x00" + s.ID + "\x00" + s.Ref))
	return hex.EncodeToString(sum[:])
}

// Find returns the song that answered this very request before, if it is still here.
func (c *Cache) Find(asked string) *online.Found {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit <= 0 {
		return nil
	}
	var best *entry
	for _, e := range c.songs {
		if slices.Contains(e.Asked, asked) && (best == nil || e.UsedMS > best.UsedMS) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.Song.Found()
}

// Opener opens a song from the cache, or with remote when it is not there.
func (c *Cache) Opener(f *online.Found, remote Opener) Opener {
	key := keyOf(online.Save(f))
	c.mu.Lock()
	e := c.songs[key]
	if e != nil && c.limit > 0 {
		e.UsedMS = time.Now().UnixMilli()
		c.save()
	} else {
		e = nil
	}
	c.mu.Unlock()
	if e == nil {
		return remote
	}
	// One play takes all its bytes from one place: the platform's may be another file.
	var local, away atomic.Bool
	return func(ctx context.Context, rng string) (*http.Response, error) {
		if away.Load() {
			return remote(ctx, rng)
		}
		wait := wakeLimit
		if local.Load() {
			wait = time.Minute
		}
		resp, err := c.awake(key, rng, wait)
		if err == nil {
			local.Store(true)
			return resp, nil
		}
		away.Store(true)
		if errors.Is(err, errAsleep) {
			c.log.Info("缓存　硬盘还没转起来，这次从音源取", "音频", f.Candidate.Title)
		} else {
			c.log.Warn("缓存　文件读不出来，改从音源取", "音频", f.Candidate.Title)
			c.drop(key)
		}
		return remote(ctx, rng)
	}
}

// wakeLimit is how long a disk that spun down may keep a song waiting.
const wakeLimit = 2500 * time.Millisecond

var errAsleep = errors.New("the disk does not answer")

// awake serves the file if the disk hands over its first bytes in time.
func (c *Cache) awake(key, rng string, wait time.Duration) (*http.Response, error) {
	type answer struct {
		resp *http.Response
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		resp, err := c.read(key, rng)
		if err == nil && resp.Body != http.NoBody {
			b := bufio.NewReaderSize(resp.Body, 64<<10)
			if _, err = b.Peek(1); err != nil {
				resp.Body.Close()
			} else {
				resp.Body = section{b, resp.Body}
			}
		}
		got <- answer{resp, err}
	}()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case a := <-got:
		return a.resp, a.err
	case <-t.C:
		go func() {
			if a := <-got; a.err == nil {
				a.resp.Body.Close()
			}
		}()
		return nil, errAsleep
	}
}

type section struct {
	io.Reader
	io.Closer
}

func (c *Cache) serve(key, rng string) (*http.Response, error) {
	c.mu.Lock()
	path := c.file(key)
	c.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := fi.Size()
	h := http.Header{"Content-Type": {"audio/mpeg"}, "Accept-Ranges": {"bytes"}}
	from, to, ok := span(rng, size)
	if !ok {
		f.Close()
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		return &http.Response{StatusCode: http.StatusRequestedRangeNotSatisfiable, Header: h, Body: http.NoBody}, nil
	}
	code := http.StatusOK
	if rng != "" {
		code = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, size))
	}
	n := to - from + 1
	h.Set("Content-Length", strconv.FormatInt(n, 10))
	return &http.Response{StatusCode: code, Header: h, ContentLength: n, Body: section{io.NewSectionReader(f, from, n), f}}, nil
}

// span reads a single byte range.
func span(rng string, size int64) (from, to int64, ok bool) {
	if rng == "" {
		return 0, size - 1, size > 0
	}
	spec, found := strings.CutPrefix(strings.ReplaceAll(rng, " ", ""), "bytes=")
	a, b, dash := strings.Cut(spec, "-")
	if !found || !dash {
		return 0, 0, false
	}
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		return max(0, size-n), size - 1, err == nil && n > 0 && size > 0
	}
	from, err := strconv.ParseInt(a, 10, 64)
	if err != nil || from < 0 || from >= size {
		return 0, 0, false
	}
	to = size - 1
	if b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n < from {
			return 0, 0, false
		}
		to = min(to, n)
	}
	return from, to, true
}

// Keep stores a song that was played, and that it answered asked.
func (c *Cache) Keep(ctx context.Context, f *online.Found, asked string, remote Opener) error {
	s := online.Save(f)
	if s.ID == "" {
		return nil
	}
	key := keyOf(s)
	c.one.Lock()
	defer c.one.Unlock()
	c.mu.Lock()
	limit, gen, e := c.limit, c.gen, c.songs[key]
	if e != nil {
		c.answer(e, asked)
		c.save()
	}
	c.mu.Unlock()
	if limit <= 0 || e != nil {
		return nil
	}
	tmp := filepath.Join(c.audio, key+".part")
	defer os.Remove(tmp)
	n, err := c.download(ctx, tmp, min(limit, maxSong), remote)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen || n > c.limit { // cleared or made smaller meanwhile
		return nil
	}
	c.trim(n)
	e = &entry{Song: s, File: freeName(c.audio, s, n, c.taken), Size: n, UsedMS: time.Now().UnixMilli()}
	c.answer(e, asked)
	c.songs[key] = e
	c.save() // first, so that no crash leaves a file nobody knows
	if err := os.Rename(tmp, c.file(key)); err != nil {
		delete(c.songs, key)
		c.save()
		return err
	}
	return nil
}

func (c *Cache) download(ctx context.Context, to string, most int64, remote Opener) (int64, error) {
	resp, err := remote(ctx, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	ctype := resp.Header.Get("Content-Type")
	size := resp.ContentLength
	switch {
	case resp.StatusCode != http.StatusOK || strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "json"):
		return 0, errors.New("来源没有给出音频")
	case size < minSong || size > most: // an unknown length cannot be checked for being complete
		return 0, errors.New("来源没有给出合适的长度")
	}
	if left, err := c.free(c.audio); err == nil && left < size+keepFree {
		return 0, errors.New("磁盘空间不多了")
	}
	f, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	var head [1]byte
	if _, err = io.ReadFull(resp.Body, head[:]); err == nil && head[0] != 'I' && head[0] != 0xFF { // ID3 or an MP3 frame
		err = errors.New("来源给的不是 MP3")
	}
	var n int64
	if err == nil {
		n, err = io.Copy(f, io.MultiReader(bytes.NewReader(head[:]), io.LimitReader(resp.Body, size)))
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil && n != size {
		err = errors.New("没有取完整")
	}
	return n, err
}

func (c *Cache) answer(e *entry, asked string) {
	for k, o := range c.songs { // the newest answer to a request is the one that counts
		if o != e {
			c.songs[k].Asked = slices.DeleteFunc(o.Asked, func(a string) bool { return a == asked })
		}
	}
	if asked != "" && !slices.Contains(e.Asked, asked) {
		e.Asked = append(e.Asked, asked)
		e.Asked = e.Asked[max(0, len(e.Asked)-maxAsked):]
	}
}

// trim makes room for extra bytes, dropping what was not used for longest. Nothing goes
// before the limit is reached.
func (c *Cache) trim(extra int64) {
	total := extra
	keys := make([]string, 0, len(c.songs))
	for k, e := range c.songs {
		total += e.Size
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return int(min(1, max(-1, c.songs[a].UsedMS-c.songs[b].UsedMS))) })
	for _, k := range keys {
		if total <= c.limit {
			return
		}
		total -= c.songs[k].Size
		os.Remove(c.file(k))
		delete(c.songs, k)
	}
}

func (c *Cache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	os.Remove(c.file(key))
	delete(c.songs, key)
	c.save()
}

func (c *Cache) save() {
	raw, err := json.Marshal(c.songs)
	if err == nil {
		tmp := c.index() + ".tmp"
		if err = os.WriteFile(tmp, raw, 0o600); err == nil {
			err = os.Rename(tmp, c.index())
		}
	}
	if err != nil {
		c.log.Warn("缓存　目录没有存上", "原因", err)
	}
}

// SetLimit changes the limit; what no longer fits goes at once.
func (c *Cache) SetLimit(mb int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limit = toBytes(mb)
	c.trim(0)
	c.save()
}

// Clear drops every song.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for k := range c.songs {
		os.Remove(c.file(k))
	}
	c.songs = map[string]*entry{}
	c.save()
}

// Status reports how full the cache is.
func (c *Cache) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{Mounted: c.mounted(), LimitMB: int(c.limit >> 20), Songs: len(c.songs)}
	if c.limit == math.MaxInt64 {
		s.LimitMB = NoLimit
	}
	for _, e := range c.songs {
		s.Bytes += e.Size
	}
	return s
}

// Tech is what a kept song really is, read from its file: an old entry does not record it.
func (c *Cache) Tech(source, id string) (online.Tech, bool) {
	c.mu.Lock()
	var key string
	var song online.Saved
	for k, e := range c.songs {
		if e.Song.Source == source && e.Song.ID == id {
			key, song = k, e.Song
			break
		}
	}
	p := ""
	if key != "" {
		p = c.file(key)
	}
	c.mu.Unlock()
	if key == "" {
		return online.Tech{}, false
	}
	if song.Kbps > 0 {
		return online.Tech{Format: "mp3", Kbps: song.Kbps, SampleRate: song.SampleRate, Bits: 16}, true
	}
	f, err := os.Open(p)
	if err != nil {
		return online.Tech{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return online.Tech{}, false
	}
	head := make([]byte, 256<<10)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	off := mp3.ID3Size(head)
	if off >= int64(n) {
		return online.Tech{}, false
	}
	m := mp3.MP3Info(head[off:], fi.Size()-off)
	if m.Kbps == 0 {
		return online.Tech{}, false
	}
	return online.Tech{Format: "mp3", Kbps: m.Kbps, SampleRate: m.SampleRate, Bits: 16}, true
}
