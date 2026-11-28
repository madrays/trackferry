package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"trackferry/internal/online"
)

func song(id string) *online.Found {
	return online.Saved{Source: "kuwo", ID: id, Ref: "r", Title: "歌" + id, Artist: "A", DurationMS: 1000}.Found()
}

func audio(n int) []byte { b := bytes.Repeat([]byte{7}, n); copy(b, "ID3"); return b }

func from(b []byte, length int64) Opener {
	return func(context.Context, string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: length, Body: io.NopCloser(bytes.NewReader(b))}, nil
	}
}

func dead(context.Context, string) (*http.Response, error) { return nil, errors.New("remote") }

func TestKeepServeAndEvict(t *testing.T) {
	ctx, dir, log := context.Background(), t.TempDir(), slog.New(slog.DiscardHandler)
	c, err := Open(dir, "", 1, log)
	if err != nil {
		t.Fatal(err)
	}
	c.free = func(string) (int64, error) { return 1 << 40, nil }
	b := audio(400 << 10)
	put := func(id string) {
		t.Helper()
		if err := c.Keep(ctx, song(id), "ask"+id, from(b, int64(len(b)))); err != nil {
			t.Fatal(err)
		}
	}
	put("1")
	put("2")
	if f := c.Find("ask1"); f == nil || f.Candidate.ID != "1" || c.Find("ask") != nil {
		t.Fatal(f)
	}
	c.songs[keyOf(online.Save(song("1")))].UsedMS = 2 // 2 is the one not used for longest
	c.songs[keyOf(online.Save(song("2")))].UsedMS = 1
	put("3")
	if c.Find("ask2") != nil || c.Find("ask1") == nil || c.Find("ask3") == nil {
		t.Fatal("the wrong song went")
	}
	// Only complete audio is kept.
	if c.Keep(ctx, song("4"), "x", from(b[:len(b)-1], int64(len(b)))) == nil ||
		c.Keep(ctx, song("4"), "x", from(b, -1)) == nil ||
		c.Keep(ctx, song("4"), "x", from(bytes.Repeat([]byte("<"), 400<<10), 400<<10)) == nil {
		t.Fatal("kept something broken")
	}
	c.free = func(string) (int64, error) { return 0, nil }
	if c.Keep(ctx, song("4"), "x", from(b, int64(len(b)))) == nil {
		t.Fatal("filled the disk")
	}
	for rng, want := range map[string][3]any{
		"":              {200, int64(len(b)), ""},
		"bytes=10-19":   {206, int64(10), "bytes 10-19/409600"},
		"bytes=409590-": {206, int64(10), "bytes 409590-409599/409600"},
		"bytes=-5":      {206, int64(5), "bytes 409595-409599/409600"},
		"bytes=409600-": {416, int64(0), "bytes */409600"},
	} {
		r, err := c.Opener(song("1"), dead)(ctx, rng)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != want[0] || int64(len(got)) != want[1] || r.Header.Get("Content-Range") != want[2] {
			t.Error(rng, r.StatusCode, len(got), r.Header)
		}
	}
	// It is all there after a restart, and leftovers are not.
	os.WriteFile(dir+"/x.part", b, 0o600)
	if c, err = Open(dir, "", 1, log); err != nil || c.Find("ask1") == nil || c.Status().Songs != 2 {
		t.Fatal(err, c.Status())
	}
	if _, err := os.Stat(dir + "/x.part"); err == nil {
		t.Fatal("leftover")
	}
	// A file that is gone is fetched again.
	open := c.Opener(song("1"), dead)
	os.Remove(c.file(keyOf(online.Save(song("1")))))
	if _, err := open(ctx, ""); err == nil || err.Error() != "remote" || c.Find("ask1") != nil {
		t.Fatal(err)
	}
	c.SetLimit(NoLimit) // keeps everything, as long as the disk has room
	had := c.Status().Songs
	for i := range 5 {
		put("more" + string(rune('a'+i)))
	}
	if s := c.Status(); s.Songs != had+5 || s.LimitMB != NoLimit {
		t.Fatal(s)
	}
	c.free = func(string) (int64, error) { return keepFree, nil }
	if c.Keep(ctx, song("full"), "x", from(b, int64(len(b)))) == nil {
		t.Fatal("kept a song on a full disk")
	}
	if again, err := Open(dir, "", NoLimit, log); err != nil || again.Status().Songs != had+5 {
		t.Fatal(err)
	}
	c.SetLimit(0)
	if c.Status().Songs != 0 || c.Keep(ctx, song("1"), "", dead) != nil {
		t.Fatal(c.Status())
	}
}

func TestNames(t *testing.T) {
	for _, x := range [][3]string{
		{"晴天", "周杰伦", "晴天-周杰伦"},
		{"晴天", "", "晴天"},
		{"", "", "未知歌曲"},
		{"../a\\b: c?", " 甲/乙 ", "&a&b c-甲&乙"},
		{"a\x00\nb\u200b", "..", "ab"},
	} {
		if got := baseName(online.Saved{Title: x[0], Artist: x[1]}); got != x[2] {
			t.Errorf("%q, want %q", got, x[2])
		}
	}
	long := baseName(online.Saved{Title: strings.Repeat("长", 300), Artist: strings.Repeat("a", 300)})
	if len(long) > 220 || !utf8.ValidString(long) {
		t.Error(len(long))
	}
	for _, n := range []string{"", ".mp3", "../x.mp3", "a/b.mp3", "x.db"} {
		if ownName(n) {
			t.Error(n)
		}
	}
}

func TestFilesAreNamedAfterTheSong(t *testing.T) {
	ctx, dir, log := context.Background(), t.TempDir(), slog.New(slog.DiscardHandler)
	b := audio(200 << 10)
	os.WriteFile(dir+"/晴天-周杰伦.mp3", []byte("mine"), 0o600)
	open := func() *Cache {
		t.Helper()
		c, err := Open(dir, "", 10, log)
		if err != nil {
			t.Fatal(err)
		}
		c.free = func(string) (int64, error) { return 1 << 40, nil }
		return c
	}
	c := open()
	has := func(name string, want bool) {
		t.Helper()
		if _, err := os.Stat(dir + "/" + name); (err == nil) != want {
			t.Fatal(name, err)
		}
	}
	for _, id := range []string{"1", "2"} { // the same song twice
		f := online.Saved{Source: "kuwo", ID: id, Title: "晴天", Artist: "周杰伦"}.Found()
		if err := c.Keep(ctx, f, "ask"+id, from(b, int64(len(b)))); err != nil {
			t.Fatal(err)
		}
	}
	has("晴天-周杰伦 (2).mp3", true)
	has("晴天-周杰伦 (3).mp3", true)
	// Kept by 1.2.0: under the key, without a name in the index.
	for k, e := range c.songs {
		os.Rename(c.file(k), dir+"/"+k+".mp3")
		e.File = ""
	}
	c.save()
	if c = open(); c.Status().Songs != 2 {
		t.Fatal(c.Status())
	}
	has("晴天-周杰伦 (2).mp3", true)
	has("晴天-周杰伦 (3).mp3", true)
	r, err := c.Opener(c.Find("ask1"), dead)(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	c.Clear()
	has("晴天-周杰伦 (2).mp3", false)
	has("晴天-周杰伦.mp3", true) // not ours
}

func TestClearStopsWhatIsDownloading(t *testing.T) {
	c, err := Open(t.TempDir(), "", 10, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	c.free = func(string) (int64, error) { return 1 << 40, nil }
	b := audio(200 << 10)
	get := from(b, int64(len(b)))
	err = c.Keep(context.Background(), song("1"), "a", func(ctx context.Context, r string) (*http.Response, error) {
		c.Clear()
		return get(ctx, r)
	})
	if err != nil || c.Status().Songs != 0 {
		t.Fatal(err, c.Status())
	}
}

func TestMountKeepsItsIndexApart(t *testing.T) {
	ctx, dir, mount, log := context.Background(), t.TempDir(), t.TempDir(), slog.New(slog.DiscardHandler)
	b := audio(200 << 10)
	keep := func(m, id string) *Cache {
		t.Helper()
		c, err := Open(dir, m, 10, log)
		if err != nil {
			t.Fatal(err)
		}
		c.free = func(string) (int64, error) { return 1 << 40, nil }
		if err := c.Keep(ctx, song(id), "ask"+id, from(b, int64(len(b)))); err != nil {
			t.Fatal(err)
		}
		return c
	}
	old := keep("", "1").file(keyOf(online.Save(song("1"))))
	os.WriteFile(mount+"/mine.mp3", b, 0o600)
	if c := keep(mount, "2"); !c.Status().Mounted || c.Find("ask1") != nil {
		t.Fatal(c.Status())
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("the old place still holds audio")
	}
	keep("", "3") // one start without the disk
	c := keep(mount, "4")
	if c.Find("ask2") == nil || c.Find("ask3") != nil {
		t.Fatal("the mount's songs were forgotten")
	}
	if _, err := os.Stat(mount + "/mine.mp3"); err != nil {
		t.Fatal("removed a file that is not ours")
	}
	// A disk that sleeps: the platform answers, for the whole play; the song stays.
	block, real := make(chan struct{}), c.read
	c.read = func(k, r string) (*http.Response, error) { <-block; return real(k, r) }
	calls := 0
	open := c.Opener(song("2"), func(ctx context.Context, r string) (*http.Response, error) {
		calls++
		return from(b, int64(len(b)))(ctx, r)
	})
	for range 2 {
		if _, err := open(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			close(block)
		}
	}
	if calls != 2 || c.Find("ask2") == nil {
		t.Fatal(calls)
	}
}

func TestUnusableMount(t *testing.T) {
	f := t.TempDir() + "/file"
	os.WriteFile(f, nil, 0o600)
	dir := t.TempDir()
	os.WriteFile(dir+"/index.json", []byte("{}"), 0o600)
	if _, err := Open(dir, f, 1, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("opened")
	}
	if _, err := os.Stat(dir + "/index.json"); err != nil {
		t.Fatal("gave up the old place")
	}
}

func TestMemberSongsAreKeptLikeAnyOther(t *testing.T) {
	c, err := Open(t.TempDir(), "", 10, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f := song("1")
	f.Candidate.Member = true
	b := audio(200 << 10)
	if err := c.Keep(context.Background(), f, "ask", from(b, int64(len(b)))); err != nil || c.Find("ask") == nil {
		t.Fatal(err, "not kept")
	}
}
