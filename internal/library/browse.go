package library

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"time"

	"trackferry/internal/names"
	"trackferry/internal/store"
)

// Album is one album of the library.
type Album struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Artist  string `json:"artist"`
	Cover   string `json:"cover"`
	Tracks  int    `json:"tracks"`
	AddedMS int64  `json:"added_ms"`
	rows    []int
}

// Artist is one artist of the library.
type Artist struct {
	Name   string `json:"name"`
	Cover  string `json:"cover"`
	Tracks int    `json:"tracks"`
}

// group derives albums and artists. Only 飞牛音乐 identifies albums.
func (x *index) group() {
	at := map[string]int{}
	for i, t := range x.rows {
		if t.AlbumID == "" {
			continue
		}
		k, ok := at[t.AlbumID]
		if !ok {
			k = len(x.albums)
			at[t.AlbumID] = k
			x.albums = append(x.albums, Album{ID: t.AlbumID, Name: t.Album, Artist: t.Artist})
		}
		a := &x.albums[k]
		a.rows = append(a.rows, i)
		a.Tracks++
		a.AddedMS = max(a.AddedMS, t.AddedMS)
		if a.Cover == "" {
			a.Cover = t.Cover
		}
		// The shortest credit is the album's main artist — but a track with no credit at all is
		// not a shorter one, and once the album takes its emptiness it never recovers.
		if a.Artist != t.Artist && len(names.Artists(t.Artist)) > 0 && len(names.Artists(t.Artist)) < len(names.Artists(a.Artist)) {
			a.Artist = t.Artist
		}
	}
	sort.SliceStable(x.albums, func(i, j int) bool { return x.albums[i].AddedMS > x.albums[j].AddedMS })
	for _, ids := range x.byArtist {
		a := Artist{Tracks: len(ids)}
		key := ""
		for _, i := range ids {
			t := x.rows[i]
			// Show the credit as written, from a song this artist sings alone.
			if as := names.Artists(t.Artist); len(as) == 1 && (a.Name == "" || t.Cover != "" && a.Cover == "") {
				a.Name, key = t.Artist, as[0]
				if t.Cover != "" {
					a.Cover = t.Cover
				}
			}
		}
		if key != "" && a.Name != "" {
			x.artists = append(x.artists, a)
		}
	}
	sort.Slice(x.artists, func(i, j int) bool {
		if x.artists[i].Tracks != x.artists[j].Tracks {
			return x.artists[i].Tracks > x.artists[j].Tracks
		}
		return x.artists[i].Name < x.artists[j].Name
	})
}

func (l *Library) index() *index {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.idx
}

// Track finds an indexed song by ID.
func (l *Library) Track(id int64) (store.TrackRow, bool) {
	x := l.index()
	i, ok := x.byID[id]
	if !ok {
		return store.TrackRow{}, false
	}
	return x.rows[i], true
}

// ByPath finds an indexed song by its path.
func (l *Library) ByPath(p string) (store.TrackRow, bool) {
	x := l.index()
	i, ok := x.byPath[p]
	if !ok {
		return store.TrackRow{}, false
	}
	return x.rows[i], true
}

// FavoriteIDs are the favourites as of the last sync.
func (l *Library) FavoriteIDs() []int64 {
	var out []int64
	for _, t := range l.index().rows {
		if t.Favorite {
			out = append(out, t.ID)
		}
	}
	return out
}

// Shelf is what the management page opens with.
type Shelf struct {
	Newest    []Album          `json:"newest"`
	Wall      []Album          `json:"wall"` // a random draw of albums with covers
	Artists   []Artist         `json:"artists"`
	Favorites []store.TrackRow `json:"favorites"`
	Picks     []store.TrackRow `json:"picks"`
	Albums    int              `json:"albums"`
}

// Shelf draws a selection.
func (l *Library) Shelf() Shelf {
	x := l.index()
	s := Shelf{Newest: []Album{}, Wall: []Album{}, Artists: []Artist{}, Favorites: []store.TrackRow{}, Picks: []store.TrackRow{}, Albums: len(x.albums)}
	var art []Album
	for _, a := range x.albums {
		if a.Cover != "" {
			art = append(art, a)
		}
	}
	s.Newest = append(s.Newest, art[:min(24, len(art))]...)
	for _, i := range rand.Perm(len(art))[:min(72, len(art))] {
		s.Wall = append(s.Wall, art[i])
	}
	// Singers with more songs are drawn more often, but not always the same ones.
	var singers []Artist
	for _, a := range x.artists {
		if a.Cover != "" && a.Tracks > 1 && len(singers) < 240 {
			singers = append(singers, a)
		}
	}
	for n := 4; len(s.Artists) < 24 && len(singers) > 0; n++ {
		i := rand.IntN(min(len(singers), 10*n))
		s.Artists = append(s.Artists, singers[i])
		singers = append(singers[:i], singers[i+1:]...)
	}
	var fav, all []store.TrackRow
	for _, t := range x.rows {
		if t.Favorite {
			fav = append(fav, t)
		}
	}
	for _, i := range rand.Perm(len(fav))[:min(24, len(fav))] {
		s.Favorites = append(s.Favorites, fav[i])
	}
	// Songs to come across: originals with a cover, one per singer.
	seen := map[string]bool{}
	for range 400 {
		if len(all) == 12 || len(x.rows) == 0 {
			break
		}
		t := x.rows[rand.IntN(len(x.rows))]
		if k := names.Norm(t.Artist); t.Cover != "" && k != "" && !seen[k] && names.VersionPenalty(t.Title, "") == 0 {
			seen[k] = true
			all = append(all, t)
		}
	}
	s.Picks = append(s.Picks, all...)
	return s
}

// ErrNotRemote is a song 飞牛音乐 does not hold.
var ErrNotRemote = errors.New("这首歌不在飞牛音乐里")

// SetFavorite adds a song to the favourites, or takes it out: in 飞牛音乐 for its songs, here
// for the music folder's (a rescan keeps them).
func (l *Library) SetFavorite(ctx context.Context, id int64, on bool) (store.TrackRow, error) {
	t, ok := l.Track(id)
	if !ok {
		return t, ErrNotRemote
	}
	if t.Favorite == on {
		return t, nil
	}
	if t.GUID != "" {
		f, can := l.Remote.(interface {
			Favorite(ctx context.Context, guid string, on bool) error
		})
		if !can {
			return t, ErrNotRemote
		}
		if err := f.Favorite(ctx, t.GUID, on); err != nil {
			return t, err
		}
	}
	t.Favorite = on
	l.mu.Lock()
	x := *l.idx
	if i, ok := x.byID[id]; ok {
		x.rows = slices.Clone(x.rows)
		x.rows[i].Favorite = on
		l.idx = &x
	}
	l.mu.Unlock()
	return t, l.st.SetFavorite(context.WithoutCancel(ctx), id, on)
}

// TVBrowse samples the catalog. The same day and batch reproduce the same shelf.
func (l *Library) TVBrowse(day time.Time, batch uint64) ([]store.TrackRow, []Album, []Artist) {
	x := l.index()
	seed := uint64(day.Year()*10000 + int(day.Month())*100 + day.Day())
	draw := rand.New(rand.NewPCG(seed, batch))
	all := draw.Perm(len(x.rows))
	recommended := make([]store.TrackRow, 0, min(12, len(all)))
	seen := make(map[string]bool)
	used := make(map[int]bool)
	for _, i := range all {
		t := x.rows[i]
		artist := names.Norm(t.Artist)
		if artist == "" || seen[artist] || names.VersionPenalty(t.Title, "") > 0 {
			continue
		}
		seen[artist], used[i] = true, true
		recommended = append(recommended, t)
		if len(recommended) == 12 {
			break
		}
	}
	for _, i := range all {
		if len(recommended) == 12 {
			break
		}
		if !used[i] {
			recommended = append(recommended, x.rows[i])
		}
	}
	albums := make([]Album, 0, min(24, len(x.albums)))
	for _, i := range draw.Perm(len(x.albums))[:cap(albums)] {
		albums = append(albums, x.albums[i])
	}
	artists := make([]Artist, 0, min(24, len(x.artists)))
	for _, i := range draw.Perm(len(x.artists))[:cap(artists)] {
		artists = append(artists, x.artists[i])
	}
	return recommended, albums, artists
}

// AlbumTracks lists an album's songs.
func (l *Library) AlbumTracks(id string) (Album, []store.TrackRow) {
	x := l.index()
	out := []store.TrackRow{}
	for _, a := range x.albums {
		if a.ID == id {
			for _, i := range a.rows {
				out = append(out, x.rows[i])
			}
			return a, out
		}
	}
	return Album{}, out
}

// FindAlbums finds albums by name, the likeliest first; artist "" is anyone's.
func (l *Library) FindAlbums(name, artist string) []Album {
	want := names.Norm(name)
	if want == "" {
		return nil
	}
	type hit struct {
		Album
		rank int
	}
	var hits []hit
	for _, a := range l.index().albums {
		if artist != "" && !names.SameArtist(a.Artist, artist) {
			continue
		}
		plain, _ := names.SplitTitle(a.Name)
		n, p := names.Norm(a.Name), names.Norm(plain)
		rank := -1
		switch {
		case n == want:
			rank = 0
		case p == want:
			rank = 1
		case strings.HasPrefix(n, want):
			rank = 2
		case strings.Contains(n, want) && len([]rune(want)) > 1:
			rank = 3
		case close1(p, want):
			rank = 4
		}
		if rank >= 0 {
			hits = append(hits, hit{a, rank})
		}
	}
	// Of albums of one name the fullest: the others are singles or loose songs.
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		return hits[i].Tracks > hits[j].Tracks
	})
	var out []Album
	for _, h := range hits {
		out = append(out, h.Album)
	}
	return out
}

// close1 reports names of three or more characters with one misheard.
func close1(a, b string) bool {
	x, y := []rune(a), []rune(b)
	if len(x) != len(y) || len(x) < 3 {
		return false
	}
	n := 0
	for i := range x {
		if x[i] != y[i] {
			n++
		}
	}
	return n == 1
}

// ArtistTracks lists an artist's songs, favourites and originals first.
func (l *Library) ArtistTracks(name string, limit int) []store.TrackRow {
	x := l.index()
	out := []store.TrackRow{}
	for _, i := range x.byArtist[names.Norm(strings.TrimSpace(name))] {
		out = append(out, x.rows[i])
	}
	score := func(t store.TrackRow) int {
		n := names.VersionPenalty(t.Title, "")
		if t.Favorite {
			n -= 5
		}
		return n
	}
	sort.SliceStable(out, func(i, j int) bool { return score(out[i]) < score(out[j]) })
	return out[:min(limit, len(out))]
}

// RandomIDs draws songs to come after one that ended: those of 飞牛音乐 if there are any.
func (l *Library) RandomIDs(n int) []int64 {
	x := l.index()
	var remote, all []int
	for i, t := range x.rows {
		all = append(all, i)
		if t.GUID != "" {
			remote = append(remote, i)
		}
	}
	if len(remote) > 0 {
		all = remote
	}
	out := []int64{}
	for _, i := range x.ordinary(all) {
		out = append(out, x.rows[i].ID)
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out[:min(n, len(out))]
}

// RandomFavoriteIDs draws songs of the favourites to come after one that ended, or of the whole
// library when nothing is a favourite.
func (l *Library) RandomFavoriteIDs(n int) []int64 {
	out := l.FavoriteIDs()
	if len(out) == 0 {
		return l.RandomIDs(n)
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out[:min(n, len(out))]
}

// ArtistIDs are the songs of an artist that are worth playing one after another.
func (l *Library) ArtistIDs(name string) []int64 {
	x := l.index()
	out := []int64{}
	for _, i := range x.ordinary(x.byArtist[names.Norm(strings.TrimSpace(name))]) {
		out = append(out, x.rows[i].ID)
	}
	return out
}

// Favorites is a page of the favourites, the newest first, and how many there are.
func (l *Library) Favorites(from, limit int) ([]store.TrackRow, int) {
	all := []store.TrackRow{}
	for _, t := range l.index().rows {
		if t.Favorite {
			all = append(all, t)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	n := len(all)
	from = max(0, min(from, n))
	return all[from:min(n, from+limit)], n
}

// Coverless lists the songs with no cover at all, grouped by album: a cover found for one is
// the album's. Songs with no album are each their own group.
func (l *Library) Coverless() [][]store.TrackRow {
	x := l.index()
	by := map[string]int{}
	var out [][]store.TrackRow
	for _, t := range x.rows {
		if t.Cover != "" {
			continue
		}
		k := t.AlbumID
		if k == "" || names.Norm(t.Album) == "" {
			out = append(out, []store.TrackRow{t})
			continue
		}
		if i, ok := by[k]; ok {
			out[i] = append(out[i], t)
			continue
		}
		by[k] = len(out)
		out = append(out, []store.TrackRow{t})
	}
	return out
}
