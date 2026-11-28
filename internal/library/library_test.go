package library

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"trackferry/internal/audio"
	"trackferry/internal/store"
)

func flac(title, artist string, secs int) []byte {
	var b bytes.Buffer
	b.WriteString("fLaC")
	si := make([]byte, 34)
	binary.BigEndian.PutUint64(si[10:], 44100<<44|1<<41|15<<36|uint64(44100*secs))
	b.Write([]byte{0, 0, 0, 34})
	b.Write(si)
	var c bytes.Buffer
	str := func(s string) { binary.Write(&c, binary.LittleEndian, uint32(len(s))); c.WriteString(s) }
	str("x")
	binary.Write(&c, binary.LittleEndian, uint32(2))
	str("TITLE=" + title)
	str("artist=" + artist)
	b.Write([]byte{0x84, 0, 0, byte(c.Len())})
	b.Write(c.Bytes())
	return b.Bytes()
}

func id3(title, artist string) []byte {
	var fr bytes.Buffer
	for id, v := range map[string]string{"TIT2": title, "TPE1": artist} {
		g, _ := simplifiedchinese.GBK.NewEncoder().String(v)
		fr.WriteString(id)
		binary.Write(&fr, binary.BigEndian, uint32(len(g)+1))
		fr.Write([]byte{0, 0, 0})
		fr.WriteString(g)
	}
	n := fr.Len()
	out := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, byte(n >> 7), byte(n & 0x7f)}
	out = append(out, fr.Bytes()...)
	frame := make([]byte, 417) // MPEG1 layer III, 128 kbps, 44.1 kHz
	copy(frame, []byte{0xff, 0xfb, 0x90, 0x00})
	for range 100 {
		out = append(out, frame...)
	}
	return out
}

func newLib(t *testing.T, files map[string][]byte) (*Library, string) {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(dir, st, slog.New(slog.DiscardHandler)), dir
}

func TestTagsAndFileNames(t *testing.T) {
	m, err := audio.ReadMeta(bytes.NewReader(flac("晴天", "周杰伦", 269)), ".flac", 0)
	if err != nil || m.Title != "晴天" || m.Artist != "周杰伦" || m.Duration != 269*time.Second || m.Bits != 16 || m.SampleRate != 44100 {
		t.Fatalf("flac: %+v %v", m, err)
	}
	raw := id3("海阔天空", "Beyond")
	m, err = audio.ReadMeta(bytes.NewReader(raw), ".mp3", int64(len(raw)))
	if err != nil || m.Title != "海阔天空" || m.Artist != "Beyond" || m.Duration < 2500*time.Millisecond || m.Duration > 2700*time.Millisecond {
		t.Fatalf("gbk id3: %+v %v", m, err)
	}
	for name, want := range map[string][2]string{
		"a/周杰伦 - 晴天.wav": {"晴天", "周杰伦"}, "01. 晴天.mp3": {"晴天", ""}, "1874.mp3": {"1874", ""}, "7 Years.mp3": {"7 Years", ""}, "03-小幸运.wav": {"小幸运", ""},
	} {
		if ti, ar := audio.FromName(name); ti != want[0] || ar != want[1] {
			t.Errorf("%s: %q %q", name, ti, ar)
		}
	}
}

func TestCorruptFilesDoNotPanic(t *testing.T) {
	good := flac("晴天", "周杰伦", 1)
	for _, src := range [][]byte{good, id3("a", "b"), []byte("RIFFxxxxWAVEfmt \xff\xff\xff\xff"), []byte("\x00\x00\x00\x08ftyp\xff\xff\xff\xffmoov")} {
		for n := range len(src) {
			if n > 600 {
				break
			}
			for _, ext := range []string{".flac", ".mp3", ".m4a", ".wav"} {
				audio.ReadMeta(bytes.NewReader(src[:n]), ext, int64(n))
			}
		}
	}
}

// A labelled cover does not make the plain song ambiguous; two plain versions do.
func TestAmbiguityIgnoresLabelledVersions(t *testing.T) {
	l, _ := newLib(t, map[string][]byte{
		"A/a.flac": flac("晴天", "周杰伦", 269),
		"A/b.flac": flac("晴天 (女声治愈版)", "某歌手", 240),
		"A/c.flac": flac("传奇", "王菲", 270),
		"A/d.flac": flac("传奇", "李健", 290),
	})
	if err := l.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := l.Song("晴天", "", "播放晴天"); r.Ambiguous || len(r.Matches) == 0 || r.Matches[0].Artist != "周杰伦" {
		t.Fatalf("plain song: %+v", r)
	}
	if r := l.Song("传奇", "", "播放传奇"); !r.Ambiguous {
		t.Fatalf("two plain songs: %+v", r)
	}
}

func TestScanAndLookup(t *testing.T) {
	ctx := context.Background()
	l, dir := newLib(t, map[string][]byte{
		"A/晴天.flac":          flac("晴天", "周杰伦", 269),
		"A/晴天live.flac":      flac("晴天 (Live)", "周杰伦", 280),
		"A/海阔天空.mp3":         id3("海阔天空", "Beyond"),
		"B/信乐团 - 海阔天空.wav":   []byte("not a wav"),
		"B/我的天空.flac":        flac("我的天空", "南征北战", 200),
		"B/@eaDir/skip.flac": flac("x", "y", 1),
		"B/cover.jpg":        {1},
	})
	if err := l.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	s := l.Status()
	if s.Tracks != 5 || len(s.Last.Roots) != 2 || s.Last.Roots[1].Damaged != 1 || s.Last.Roots[1].ByName != 1 {
		t.Fatalf("status %+v %+v", s, s.Last)
	}
	if r := l.Song("晴天", "周杰倫", "播放周杰伦的晴天"); len(r.Matches) != 2 || r.Matches[0].Title != "晴天" {
		t.Fatalf("studio version first, traditional artist: %+v", r)
	}
	if r := l.Song("海阔天空", "", ""); !r.Ambiguous || len(r.Artists) != 2 {
		t.Fatalf("want ambiguous: %+v", r)
	}
	if r := l.Song("海阔天空", "beyond", ""); r.Ambiguous || len(r.Matches) != 1 {
		t.Fatalf("artist decides: %+v", r)
	}
	for _, q := range []string{"晴天 周杰伦", "周杰伦晴天", "晴天周杰伦"} {
		if r, title, artist := l.Split(q, q); len(r.Matches) == 0 || title != "晴天" || artist != "周杰伦" {
			t.Errorf("split %q: %q %q %d", q, title, artist, len(r.Matches))
		}
	}
	if m, n := l.Artist("周杰伦"); n != 2 || m.Title != "晴天" {
		t.Fatalf("artist pick: %+v %d", m, n)
	}

	// Second scan: nothing reread, a deleted file goes, an unreadable root keeps its tracks.
	os.Remove(filepath.Join(dir, "A/晴天live.flac"))
	os.Chmod(filepath.Join(dir, "B"), 0)
	defer os.Chmod(filepath.Join(dir, "B"), 0o755)
	if err := l.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	s = l.Status()
	if s.Last.Added != 0 || s.Last.Removed != 1 {
		t.Fatalf("rescan: %+v", s.Last)
	}
	if os.Getuid() != 0 && (s.Last.Roots[1].Error == "" || s.Tracks != 4) {
		t.Fatalf("unreadable root: %+v tracks %d", s.Last.Roots[1], s.Tracks)
	}
}

// A search that matches nothing as typed still finds what was meant by one misheard character.
func TestFindToleratesAMisheardCharacter(t *testing.T) {
	l, _ := newLib(t, map[string][]byte{
		"A/a.flac": flac("乌兰巴托的夜", "洋澜一", 240),
		"A/b.flac": flac("哭砂", "洋澜一", 250),
		"A/c.flac": flac("晴天", "周杰伦", 269),
	})
	if err := l.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ms := l.Find("洋澜一", 50); len(ms) != 2 {
		t.Fatalf("as typed: %d", len(ms))
	}
	ms := l.Find("杨澜一", 50) // 洋 written as its homophone 杨, as recognition often does
	if len(ms) != 2 || ms[0].Artist != "洋澜一" {
		t.Fatalf("near singer: %+v", ms)
	}
	if ms := l.Find("乌兰巴托之夜", 50); len(ms) != 1 || ms[0].Title != "乌兰巴托的夜" {
		t.Fatalf("near title: %+v", ms)
	}
	if ms := l.Find("张三李四", 50); len(ms) != 0 {
		t.Fatalf("nothing near: %+v", ms)
	}
}
