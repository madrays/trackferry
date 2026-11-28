package routing

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"trackferry/internal/intent"
	"trackferry/internal/library"
	"trackferry/internal/online"
	"trackferry/internal/store"
)

// shelf is a platform with a few songs, every one of them full length.
type shelf struct {
	url   string
	songs []online.Candidate
	mu    sync.Mutex
	asked []string
}

func (s *shelf) Name() string { return "kuwo" }
func (s *shelf) Search(_ context.Context, q string) ([]online.Candidate, error) {
	s.mu.Lock()
	s.asked = append(s.asked, q)
	s.mu.Unlock()
	var out []online.Candidate
	for _, c := range s.songs {
		if strings.Contains(c.Title, q) || strings.Contains(c.Artist, q) {
			out = append(out, c)
		}
	}
	return out, nil
}
func (s *shelf) Resolve(_ context.Context, c online.Candidate) (string, error) {
	if c.ID == "broken" {
		return "", online.ErrNoAudio
	}
	return s.url + "/audio/" + c.ID, nil
}

func offered(id, title, artist string, restricted bool) online.Candidate {
	return online.Candidate{Source: "kuwo", ID: id, Title: title, Artist: artist, Duration: 200 * time.Second, DurationMS: 200_000, Restricted: restricted}
}

func shelfRouter(t *testing.T, files []string, songs ...online.Candidate) (*Router, *shelf) {
	t.Helper()
	ctx, log := context.Background(), slog.New(slog.DiscardHandler)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 200*16000) // 200 s at 128 kbps
		for i := 0; i+417 <= len(b); i += 417 {
			copy(b[i:], []byte{0xff, 0xfb, 0x90, 0})
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(b)))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	for _, n := range files {
		os.WriteFile(filepath.Join(dir, n+".mp3"), []byte("no tags in this little file"), 0o644)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	lib := library.New(dir, st, log)
	if err := lib.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	sh := &shelf{url: srv.URL, songs: songs}
	return &Router{Library: lib, Online: online.New(log, srv.Client(), sh), Log: log}, sh
}

func TestSingerNotInTheLibraryPlaysOnline(t *testing.T) {
	r, _ := shelfRouter(t, []string{"周杰伦 - 晴天"},
		offered("v", "十年", "陈奕迅", true),       // members only
		offered("broken", "浮夸", "陈奕迅", false), // no audio
		offered("1", "好久不见", "陈奕迅", false),
		offered("2", "十年 (Live)", "陈奕迅", false), // a live one after the plain ones
		offered("3", "富士山下", "陈奕迅", false),
		offered("4", "好久不见", "陈奕迅", false), // the same title twice
		offered("5", "陈奕迅的歌", "别人", false)) // someone else
	rt := r.Decide(context.Background(), intent.Parse("播放陈奕迅的歌"), "播放陈奕迅的歌")
	if rt.Decision != Online || rt.Title != "好久不见" || rt.List != "陈奕迅的歌" {
		t.Fatalf("%+v", rt)
	}
	var got []string
	for _, c := range rt.Ahead() {
		got = append(got, c.Title)
	}
	if strings.Join(got, ",") != "富士山下,十年 (Live),浮夸" {
		t.Fatalf("plays on with %v", got)
	}
	if rt := r.Decide(context.Background(), intent.Parse("播放周杰伦的歌"), ""); rt.Decision != Local {
		t.Fatalf("the library's singer: %+v", rt)
	}
	if rt := r.Decide(context.Background(), intent.Parse("播放孙燕姿的歌"), ""); rt.Decision != Native || rt.Reason != "曲库没有孙燕姿，线上也没找到" {
		t.Fatalf("nowhere: %+v", rt)
	}
}

func TestOneMisheardSongOfTheNamedSingerNeedsNoPlatform(t *testing.T) {
	r, sh := shelfRouter(t, []string{"周杰伦 - 晴天", "Beyond - 海阔天空", "信乐团 - 海阔天空"}, offered("1", "晴天", "周杰伦", false))
	rt := r.Decide(context.Background(), intent.Parse("播放Beyond的海扩天空"), "播放Beyond的海扩天空")
	if rt.Decision != Local || rt.Artist != "Beyond" || len(sh.asked) != 0 {
		t.Fatalf("%+v asked %v", rt, sh.asked)
	}
	// No singer named, two to choose from: the platforms still have their say.
	rt = r.Decide(context.Background(), intent.Parse("播放海扩天空"), "播放海扩天空")
	if rt.Decision != Local || len(sh.asked) == 0 {
		t.Fatalf("%+v asked %v", rt, sh.asked)
	}
	// Said as it is on a platform: that song, not the library's near one.
	rt = r.Decide(context.Background(), intent.Parse("播放晴天"), "播放晴天")
	if rt.Decision != Local || rt.Artist != "周杰伦" {
		t.Fatalf("%+v", rt)
	}
}
