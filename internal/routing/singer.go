package routing

import (
	"context"
	"fmt"
	"time"

	"trackferry/internal/intent"
	"trackferry/internal/live"
	"trackferry/internal/names"
	"trackferry/internal/online"
)

const (
	// singerWait bounds looking a singer up on the platforms; Xiao AI talks meanwhile.
	singerWait = 8 * time.Second
	// singerSongs is how many of a singer's online songs play on, round and round.
	singerSongs = 20
	// singerTries is how many songs may fail to play before the first one is given up.
	singerTries = 3
)

// singer plays a singer the library does not have from the platforms: the first of their songs
// that can be played in full, then the others the platforms showed, one after another.
func (r *Router) singer(ctx context.Context, rt *Route, in intent.Intent) {
	start := time.Now()
	tr := live.From(ctx)
	ctx, cancel := context.WithTimeout(ctx, singerWait)
	defer cancel()
	cands, outs := r.Online.Search(ctx, in.Artist)
	songs := songsOf(cands, in.Artist)
	for _, o := range outs {
		l := Layer{Name: o.Source, State: o.State, MS: o.MS, Detail: o.Detail, Candidates: []Candidate{}}
		n := 0
		for _, c := range songs {
			if c.Source == o.Source {
				if n < 6 {
					l.Candidates = append(l.Candidates, Candidate{Title: c.Title, Artist: c.Artist, DurationMS: c.DurationMS})
				}
				n++
			}
		}
		if l.State == online.StateHit && n == 0 {
			l.State, l.Detail = online.StateMiss, "搜到的不是"+in.Artist+"的歌"
		}
		rt.Layers = append(rt.Layers, l)
		tr.Layer(o.Source, l)
	}
	for i, c := range songs[:min(singerTries, len(songs))] {
		f, err := r.Online.Take(ctx, c.Key)
		if err != nil {
			continue
		}
		rt.takeOnline(f)
		rt.Reason = ""
		rest := append(append([]online.Candidate{}, songs[i+1:]...), songs[:i]...)
		rt.ahead = rest[:min(singerSongs-1, len(rest))]
		why := fmt.Sprintf("曲库没有%s，线上找到 %d 首，连着放", in.Artist, len(rt.ahead)+1)
		if len(rt.ahead) == 0 {
			why = "曲库没有" + in.Artist + "，线上只找到这一首"
		} else {
			rt.List = in.Artist + "的歌"
		}
		for j := range rt.Layers {
			if rt.Layers[j].Name == c.Source {
				rt.Layers[j].State, rt.Layers[j].Detail = online.StateHit, why
			}
		}
		rt.Because("选它", "%s", why)
		r.Log.Info("点歌　按歌手找线上", "歌手", in.Artist, "首数", len(rt.ahead)+1, "耗时", time.Since(start).Round(time.Millisecond))
		return
	}
	switch {
	case len(songs) > 0:
		rt.Reason = "曲库没有" + in.Artist + "，线上的歌暂时放不了"
	case len(cands) > 0:
		rt.Reason = "曲库没有" + in.Artist + "，线上只有会员或别人的歌"
	default:
		rt.Reason = "曲库没有" + in.Artist + "，线上也没找到"
	}
}

// songsOf keeps the songs of a singer that can be played in full, each title once, the plain
// versions before live recordings, accompaniments and the like.
func songsOf(cands []online.Candidate, artist string) []online.Candidate {
	var plain, other []online.Candidate
	seen := map[string]bool{}
	for _, c := range cands {
		t := names.Norm(c.Title)
		if c.Restricted || t == "" || seen[t] || !names.ArtistMatch(c.Artist, artist) {
			continue
		}
		seen[t] = true
		if len(names.Versions(c.Title)) > 0 {
			other = append(other, c)
		} else {
			plain = append(plain, c)
		}
	}
	return append(plain, other...)
}
