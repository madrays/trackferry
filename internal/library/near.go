package library

import (
	"slices"
	"sort"
	"strings"

	"trackferry/internal/names"
)

// Near looks a song up that may have been misheard by a character, in its title or its singer.
// The best are first; titles that fit equally well are all there.
func (l *Library) Near(title, artist, request string) Result {
	l.mu.RLock()
	x := l.idx
	l.mu.RUnlock()
	want := names.Norm(title)
	if want == "" {
		return Result{}
	}
	if artist != "" {
		return x.nearBy(title, artist, request)
	}
	// "周杰伦龙卷峰", with or without a space: the singer is one of the library.
	if f := strings.Fields(title); len(f) > 1 {
		for i := 1; i < len(f); i++ {
			a, b := strings.Join(f[:i], " "), strings.Join(f[i:], " ")
			for _, p := range [][2]string{{b, a}, {a, b}} {
				if r := x.nearBy(p[0], p[1], request); len(r.Matches) > 0 {
					return r
				}
			}
		}
	}
	rs := []rune(strings.ReplaceAll(title, " ", ""))
	for i := len(rs) - 2; i > 1; i-- {
		if r := x.nearBy(string(rs[i:]), string(rs[:i]), request); len(r.Matches) > 0 {
			return r
		}
	}
	return x.gather(x.titlesNear(want, false), "", request)
}

// nearBy allows for one of both to have been misheard, not for both.
func (x *index) nearBy(title, artist, request string) Result {
	want := names.Norm(title)
	if len(x.byArtist[names.Norm(artist)]) > 0 {
		return x.gather(x.titlesNear(want, true), artist, request)
	}
	if want == "" {
		return Result{}
	}
	all := x.song(title, "", request)
	var ms []Match
	for _, m := range all.Matches {
		if names.CreditNear(m.Artist, artist) {
			ms = append(ms, m)
		}
	}
	return result(ms)
}

func (x *index) titlesNear(want string, singer bool) []string {
	var out []string
	for k := range x.byTitle {
		if names.NearTitle(k, want, singer) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func (x *index) gather(titles []string, artist, request string) Result {
	var ms []Match
	for _, k := range titles {
		for _, m := range x.song(k, artist, request).Matches {
			if !slices.ContainsFunc(ms, func(o Match) bool { return o.ID == m.ID }) {
				ms = append(ms, m)
			}
		}
	}
	return result(ms)
}

func result(ms []Match) Result {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Score < ms[j].Score })
	res := Result{Matches: ms}
	res.Artists, res.Ambiguous = groupArtists(ms)
	return res
}

// NearArtist is the one singer of the library whose name differs from name by a misheard
// character; "" if there is none, or more than one.
func (l *Library) NearArtist(name string) string {
	l.mu.RLock()
	x := l.idx
	l.mu.RUnlock()
	want, found := names.Norm(name), ""
	for a, ids := range x.byArtist {
		if !names.NearArtist(a, want) {
			continue
		}
		if found != "" {
			return ""
		}
		found = a
		for _, p := range strings.FieldsFunc(x.rows[ids[0]].Artist, func(r rune) bool { return strings.ContainsRune("/&、,，;；", r) }) {
			if names.Norm(p) == a {
				found = strings.TrimSpace(p) // as it is written
			}
		}
	}
	return found
}

// nearTerms gathers the songs whose title or singer differs from want by one misheard
// character, for a search that matched nothing as typed.
func (x *index) nearTerms(want string) []Match {
	seen := map[int]bool{}
	var ms []Match
	add := func(ids []int) {
		for _, i := range ids {
			if !seen[i] {
				seen[i] = true
				ms = append(ms, Match{TrackRow: x.rows[i], Score: formatRank(x.rows[i])})
			}
		}
	}
	for _, k := range x.titlesNear(want, false) {
		add(x.byTitle[k])
	}
	for a, ids := range x.byArtist {
		if names.NearArtist(a, want) {
			add(ids)
		}
	}
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Score != ms[j].Score {
			return ms[i].Score < ms[j].Score
		}
		return ms[i].ID < ms[j].ID
	})
	return ms
}
