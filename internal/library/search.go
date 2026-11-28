package library

import (
	"math/rand/v2"
	"sort"
	"strings"

	"trackferry/internal/names"
	"trackferry/internal/store"
)

// index is immutable once built.
type index struct {
	rows     []store.TrackRow
	byTitle  map[string][]int // normalised full and plain titles
	byArtist map[string][]int
	byPath   map[string]int
	byID     map[int64]int
	hay      []string        // normalised title, artist and album, for Find
	albums   []Album         // newest first
	artists  []Artist        // most songs first
	covers   map[string]bool // local cover IDs some song uses
}

func newIndex(rows []store.TrackRow) *index {
	x := &index{rows: rows, byTitle: map[string][]int{}, byArtist: map[string][]int{},
		byPath: make(map[string]int, len(rows)), byID: make(map[int64]int, len(rows)), hay: make([]string, len(rows)), covers: map[string]bool{}}
	for i, t := range rows {
		x.byPath[t.Path], x.byID[t.ID] = i, i
		if strings.HasPrefix(t.Cover, LocalCover) {
			x.covers[t.Cover] = true
		}
		x.hay[i] = names.Norm(t.Title) + "\x00" + names.Norm(t.Artist) + "\x00" + names.Norm(t.Album)
		keys := map[string]bool{names.Norm(t.Title): true}
		plain, _ := names.SplitTitle(t.Title)
		keys[names.Norm(plain)] = true
		if !t.Tagged {
			// "歌名 - 歌手" naming is common too: find the file by either half.
			keys[names.Norm(t.Artist)] = true
		}
		for k := range keys {
			if k != "" {
				x.byTitle[k] = append(x.byTitle[k], i)
			}
		}
		seen := map[string]bool{}
		for _, a := range names.Artists(t.Artist) {
			if !seen[a] {
				seen[a] = true
				x.byArtist[a] = append(x.byArtist[a], i)
			}
		}
	}
	x.group()
	return x
}

// Match is one file that fits a request. Lower Score is better.
type Match struct {
	store.TrackRow
	Score int `json:"score"`
}

// Result of looking a song up.
type Result struct {
	Matches   []Match // best first
	Ambiguous bool    // several artists sing it and the request names none
	Artists   []string
}

// formatRank prefers what every speaker decodes.
func formatRank(t store.TrackRow) int {
	r := map[string]int{"mp3": 0, "flac": 1, "wav": 2, "m4a": 3, "aac": 3, "ogg": 4, "alac": 8}[t.Format]
	if t.SampleRate > 48000 || t.Bits > 16 {
		r += 4 // hi-res: some models refuse it
	}
	return r
}

// Song looks a song up. request is the whole utterance (versions it mentions are allowed).
func (l *Library) Song(title, artist, request string) Result {
	l.mu.RLock()
	x := l.idx
	l.mu.RUnlock()
	return x.song(title, artist, request)
}

func (x *index) song(title, artist, request string) Result {
	want := names.Norm(title)
	if want == "" {
		return Result{}
	}
	var ms []Match
	for _, i := range x.byTitle[want] {
		t := x.rows[i]
		if !t.Tagged && names.Norm(t.Artist) == want && names.Norm(t.Title) != want {
			if p, _ := names.SplitTitle(t.Title); names.Norm(p) != want {
				// The file is named "歌名 - 歌手". Only trust the swap when an artist confirms it.
				if artist == "" || !names.ArtistMatch(t.Title, artist) {
					continue
				}
				t.Title, t.Artist = t.Artist, t.Title
			}
		}
		if artist != "" && !names.ArtistMatch(t.Artist, artist) {
			continue
		}
		score := formatRank(t) + 20*names.VersionPenalty(t.Title+" "+t.Album, request)
		if names.Norm(t.Title) != want {
			score += 10
		}
		if t.Favorite {
			score -= 5
		}
		ms = append(ms, Match{TrackRow: t, Score: score})
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Score < ms[j].Score })
	res := Result{Matches: ms}
	if artist == "" {
		// A cover that says so in its title ("晴天 (女声治愈版)") does not rival the plain song.
		plain := []Match{}
		for _, m := range ms {
			if names.Norm(m.Title) == want && names.VersionPenalty(m.Title+" "+m.Album, request) == 0 {
				plain = append(plain, m)
			}
		}
		if len(plain) == 0 {
			plain = ms
		}
		res.Artists, res.Ambiguous = groupArtists(plain)
	}
	return res
}

// groupArtists merges credits that share a name ("周杰伦" and "周杰伦/费玉清" are one singer's
// song) and reports whether more than one group is left.
func groupArtists(ms []Match) ([]string, bool) {
	var sets []map[string]bool
	var credits []string
next:
	for _, m := range ms {
		as := names.Artists(m.Artist)
		if len(as) == 0 {
			continue
		}
		for _, s := range sets {
			for _, a := range as {
				if s[a] {
					for _, b := range as {
						s[b] = true
					}
					continue next
				}
			}
		}
		s := map[string]bool{}
		for _, a := range as {
			s[a] = true
		}
		sets = append(sets, s)
		credits = append(credits, m.Artist)
	}
	return credits, len(credits) > 1
}

// Split tries to read text as artist and title in either order, with or without a space
// ("周杰伦晴天", "晴天 周杰伦"), and returns the first reading the library confirms.
func (l *Library) Split(text, request string) (Result, string, string) {
	l.mu.RLock()
	x := l.idx
	l.mu.RUnlock()
	try := func(artist, title string) (Result, bool) {
		if len(x.byArtist[names.Norm(artist)]) == 0 {
			return Result{}, false
		}
		r := x.song(title, artist, request)
		return r, len(r.Matches) > 0
	}
	if f := strings.Fields(text); len(f) > 1 {
		for i := 1; i < len(f); i++ {
			a, b := strings.Join(f[:i], " "), strings.Join(f[i:], " ")
			if r, ok := try(a, b); ok {
				return r, b, a
			}
			if r, ok := try(b, a); ok {
				return r, a, b
			}
		}
	}
	rs := []rune(strings.ReplaceAll(text, " ", ""))
	for i := len(rs) - 1; i > 0; i-- {
		a, b := string(rs[:i]), string(rs[i:])
		if len([]rune(a)) >= 2 {
			if r, ok := try(a, b); ok {
				return r, b, a
			}
		}
		if len([]rune(b)) >= 2 {
			if r, ok := try(b, a); ok {
				return r, a, b
			}
		}
	}
	return Result{}, "", ""
}

// Artist returns one random song by an artist, plus how many there are.
func (l *Library) Artist(name string) (Match, int) {
	l.mu.RLock()
	x := l.idx
	l.mu.RUnlock()
	return x.pick(x.byArtist[names.Norm(name)])
}

// Random returns any song.
func (l *Library) Random() (Match, int) {
	l.mu.RLock()
	x := l.idx
	l.mu.RUnlock()
	all := make([]int, len(x.rows))
	for i := range all {
		all[i] = i
	}
	return x.pick(all)
}

// pick prefers ordinary songs: 1–12 minutes, no karaoke versions, easily decoded.
func (x *index) pick(ids []int) (Match, int) {
	if len(ids) == 0 {
		return Match{}, 0
	}
	good := x.ordinary(ids)
	return Match{TrackRow: x.rows[good[rand.IntN(len(good))]]}, len(ids)
}

// ordinary keeps ordinary songs, or all if there are none.
func (x *index) ordinary(ids []int) []int {
	var good []int
	for _, i := range ids {
		t := x.rows[i]
		if (t.DurationMS == 0 || t.DurationMS > 60_000 && t.DurationMS < 720_000) && names.VersionPenalty(t.Title, "") == 0 && formatRank(t) < 4 {
			good = append(good, i)
		}
	}
	if len(good) == 0 {
		good = ids
	}
	return good
}

// Find is the free-text search of the management page.
func (l *Library) Find(q string, limit int) []Match {
	l.mu.RLock()
	x := l.idx
	l.mu.RUnlock()
	var terms []string
	for _, f := range strings.Fields(q) {
		if n := names.Norm(f); n != "" {
			terms = append(terms, n)
		}
	}
	out := []Match{}
	if len(terms) == 0 {
		return out
	}
	for i, t := range x.rows {
		hay := x.hay[i]
		ok, score := true, formatRank(t)
		for _, term := range terms {
			if !strings.Contains(hay, term) {
				ok = false
				break
			}
			if names.Norm(t.Title) != term && names.Norm(t.Artist) != term {
				score += 10
			}
		}
		if ok {
			out = append(out, Match{TrackRow: t, Score: score})
		}
	}
	if len(out) == 0 {
		// Nothing fits as typed: what was meant may differ by one misheard character, as the
		// spoken lookups already allow. A singer's name, or a title long enough to go by alone.
		out = x.nearTerms(strings.Join(terms, ""))
	}
	return out[:min(limit, len(out))]
}

// Count is how many songs carry any of the credited artists.
func (l *Library) Count(artist string) int {
	l.mu.RLock()
	x := l.idx
	l.mu.RUnlock()
	n := 0
	for _, a := range names.Artists(artist) {
		n = max(n, len(x.byArtist[a]))
	}
	return n
}
