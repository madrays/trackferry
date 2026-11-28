package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"trackferry/internal/feiniu"
	"trackferry/internal/observe"
	"trackferry/internal/online"
	"trackferry/internal/routing"
	"trackferry/internal/store"
	"trackferry/internal/takeover"
	"trackferry/internal/xiaomi"
)

// target is the speaker of a request that plays something on it.
func (s *Server) target(w http.ResponseWriter, r *http.Request) (store.Device, bool) {
	if !s.Xiaomi.Status().Linked {
		writeError(w, http.StatusConflict, "not_linked", "还没有关联小米账号")
		return store.Device{}, false
	}
	d, err := s.Store.Device(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no_device", "没有这台音箱")
		return d, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return d, false
	}
	if !d.InAccount || d.Mode == store.ModeOff {
		writeError(w, http.StatusConflict, "device_off", "这台音箱没有接入。先在音箱页把它设为「观察」")
		return d, false
	}
	if err := s.Stream.Ready(); err != nil {
		writeError(w, http.StatusConflict, "no_public_url", "音箱不知道从哪里取音频：请在 config.yaml 里设置 public_base_url（音箱能访问到的 NAS 地址，例如 http://192.168.1.10:18561）后重启")
		return d, false
	}
	return d, true
}

// play sends a list, or an online song, to a speaker.
func (s *Server) play(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string  `json:"source"` // favorites | playlist | artist | album | picks | track | online
		ID     string  `json:"id"`     // the playlist, album or online song
		Name   string  `json:"name"`   // the artist
		Track  int64   `json:"track_id"`
		Tracks []int64 `json:"track_ids"` // the discovery draw already on screen
		Title  string  `json:"title"`
		Artist string  `json:"artist"`
		Next   bool    `json:"next"` // after the song that is on
		Last   bool    `json:"last"` // add a library song at the end of the queue
	}
	if !readJSON(w, r, &req) {
		return
	}
	d, ok := s.target(w, r)
	if !ok {
		return
	}
	if req.Last && (req.Next || req.Source != "track") {
		writeError(w, http.StatusBadRequest, "bad_source", "只有曲库歌曲可以放到队尾")
		return
	}
	if req.Source == "track" {
		rt, ok := s.Router.Next(req.Track, "")
		if !ok {
			writeError(w, http.StatusNotFound, "no_track", "这首歌暂时读不到")
			return
		}
		if (req.Next || req.Last) && s.Dispatcher.QueueTrack(d.DeviceID, req.Track, req.Next) {
			writeJSON(w, http.StatusOK, map[string]bool{"queued": true})
			return
		}
		s.answerRun(w, r, d)(s.Dispatcher.Push(r.Context(), d, rt))
		return
	}
	if req.Next {
		w0 := takeover.Wish{Title: req.Title, Artist: req.Artist}
		switch req.Source {
		case "online":
			w0.Route = func(ctx context.Context) (*routing.Route, error) {
				f, err := s.Router.Online.Take(ctx, req.ID)
				if err != nil {
					return nil, err
				}
				return routing.FromOnline(f), nil
			}
		default:
			writeError(w, http.StatusBadRequest, "bad_source", "只有单首歌可以排到下一首")
			return
		}
		if len(w0.Title) > 300 {
			w0.Title = ""
		}
		if len(w0.Artist) > 300 {
			w0.Artist = ""
		}
		queued, err := s.Dispatcher.Want(d.DeviceID, w0)
		if err != nil {
			writeError(w, http.StatusConflict, "full", err.Error())
			return
		}
		if queued {
			writeJSON(w, http.StatusOK, map[string]bool{"queued": true})
			return
		}
		rt, err := w0.Route(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, "no_audio", "这首放不了："+err.Error())
			return
		}
		s.answerRun(w, r, d)(s.Dispatcher.Push(r.Context(), d, rt))
		return
	}
	if req.Source == "online" {
		found, err := s.Router.Online.Take(r.Context(), req.ID)
		if errors.Is(err, online.ErrGone) {
			writeError(w, http.StatusNotFound, "gone", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusBadGateway, "no_audio", "这首放不了："+err.Error())
			return
		}
		s.answerRun(w, r, d)(s.Dispatcher.Push(r.Context(), d, routing.FromOnline(found)))
		return
	}
	list, ids, ok := s.collect(w, r, req.Source, req.ID, req.Name, req.Tracks)
	if !ok {
		return
	}
	if req.Track != 0 && !slices.Contains(ids, req.Track) {
		writeError(w, http.StatusConflict, "track_moved", "这首已不在播放列表里，刷新后再试")
		return
	}
	s.answerRun(w, r, d)(s.Dispatcher.PlayList(r.Context(), d, list, ids, req.Track))
}

// afterRandom is how many songs of the library at random follow songs that were not picked as a
// whole: a single one or the few that happened to be shown. The same on every output.
const afterRandom = 500

// fullList is how many random songs the browser and the television draw at a time to go on with.
const fullList = 100

// following draws what follows songs that were not picked as a whole, as the settings say.
func (s *Server) following(n int) []int64 {
	return takeover.Following(context.Background(), s.Store, s.Library, n)
}

// withRandom has random songs follow one, as the settings say.
func (s *Server) withRandom(first int64) []int64 {
	ids := []int64{first}
	for _, id := range s.following(afterRandom) {
		if id != first {
			ids = append(ids, id)
		}
	}
	return ids
}

// collect lists the songs of something that can be played through.
func (s *Server) collect(w http.ResponseWriter, r *http.Request, source, id, name string, picks []int64) (list string, ids []int64, ok bool) {
	return s.collectTracks(w, r, source, id, name, picks, true)
}

func (s *Server) collectTracks(w http.ResponseWriter, r *http.Request, source, id, name string, picks []int64, deduplicate bool) (list string, ids []int64, ok bool) {
	switch source {
	case "favorites":
		list = "我的收藏"
		tracks, _ := s.Library.Favorites(0, math.MaxInt)
		for _, t := range tracks {
			ids = append(ids, t.ID)
		}
	case "playlist":
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		got, all, err := s.Router.Collect(ctx, id)
		if err != nil {
			writeError(w, http.StatusBadGateway, "feiniu", err.Error())
			return
		}
		list, ids = "歌单“"+got+"”", all
	case "artist":
		list = name + "的歌"
		for _, t := range s.Library.ArtistTracks(name, math.MaxInt) {
			ids = append(ids, t.ID)
		}
	case "album":
		a, ts := s.Library.AlbumTracks(id)
		list = "专辑“" + a.Name + "”"
		for _, t := range ts {
			ids = append(ids, t.ID)
		}
	case "picks":
		if len(picks) > 12 {
			writeError(w, http.StatusBadRequest, "bad_list", "随手翻到的最多 12 首")
			return
		}
		list = "随手翻到的"
		for _, id := range picks {
			if _, ok := s.Library.Track(id); ok {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 { // picks go on as anything else not picked as a whole
			for _, id := range s.withRandom(ids[0])[1:] {
				if !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_source", "不支持这种播放来源")
		return
	}
	if deduplicate {
		seen := make(map[int64]bool, len(ids))
		unique := make([]int64, 0, len(ids))
		for _, id := range ids {
			if !seen[id] {
				unique = append(unique, id)
				seen[id] = true
			}
		}
		ids = unique
	}
	if len(ids) == 0 {
		writeError(w, http.StatusNotFound, "empty", list+"里没有可放的歌")
		return
	}
	return list, ids, true
}

// answerRun tells how a push went. It is called before the push, so it sees who had the
// speaker: one browser taking it from another is told so.
func (s *Server) answerRun(w http.ResponseWriter, r *http.Request, d store.Device) func(store.TakeoverRun, error) {
	me, had := s.who(r), s.Dispatcher.By(d.DeviceID)
	return func(run store.TakeoverRun, err error) {
		if err != nil {
			writeError(w, http.StatusConflict, "no_audio", err.Error())
			return
		}
		s.Dispatcher.Tag(d.DeviceID, me)
		run.DeviceName = firstNonEmpty(d.Alias, d.Name)
		writeJSON(w, http.StatusAccepted, struct {
			store.TakeoverRun
			Took string `json:"took,omitempty"` // the browser whose song this one replaced
		}{run, took(me, had)})
	}
}

// took is the other browser a song was taken from; "" when it was this one or none.
func took(me, had string) string {
	if had == me {
		return ""
	}
	return had
}

func (s *Server) playing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, takeover.LoadPrefs(r.Context(), s.Store))
}

func (s *Server) playingUpdate(w http.ResponseWriter, r *http.Request) {
	p := takeover.LoadPrefs(r.Context(), s.Store) // what is left out stays
	if !readJSON(w, r, &p) {
		return
	}
	if p.Pool != "library" && p.Pool != "favorites" {
		writeError(w, http.StatusBadRequest, "bad_pool", "只能从整个曲库或我的收藏里挑")
		return
	}
	if p.Hours < 0 || p.Hours > 24 {
		writeError(w, http.StatusBadRequest, "bad_hours", "时间要在 0 到 24 小时之间，0 是不限")
		return
	}
	if err := takeover.SavePrefs(r.Context(), s.Store, p); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) listening(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, observe.LoadPrefs(r.Context(), s.Store))
}

func (s *Server) listeningUpdate(w http.ResponseWriter, r *http.Request) {
	p := observe.LoadPrefs(r.Context(), s.Store) // what is left out stays
	if !readJSON(w, r, &p) {
		return
	}
	if bad := p.Check(); bad != "" {
		writeError(w, http.StatusBadRequest, "bad_listening", bad)
		return
	}
	if err := observe.SavePrefs(r.Context(), s.Store, p); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if s.Observer != nil {
		s.Observer.Apply(p)
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) deviceHush(w http.ResponseWriter, r *http.Request, hush string) {
	d, err := s.Store.Device(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_device", "没有这台音箱")
		return
	}
	switch {
	case hush != takeover.HushChosenNormal && hush != takeover.HushStrong && hush != takeover.HushWait:
		writeError(w, http.StatusBadRequest, "bad_hush", "hush 只能是 normal、strong 或 wait")
		return
	case hush == takeover.HushStrong && !xiaomi.CanHush(d.Hardware, d.MiotDID):
		writeError(w, http.StatusConflict, "cannot_hush", "这个型号不能通过米家静音")
		return
	}
	if err := s.Store.SetDeviceHush(r.Context(), d.DeviceID, hush); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if hush != takeover.HushChosenNormal && xiaomi.CanHush(d.Hardware, d.MiotDID) {
		go s.Xiaomi.WarmHome(context.WithoutCancel(r.Context()))
	}
	d.Hush = hush
	s.Log.Info("音箱　改了静音方式", "音箱", d.Name, "方式", hush)
	writeJSON(w, http.StatusOK, s.deviceView(d))
}

func (s *Server) deviceMP3(w http.ResponseWriter, r *http.Request, on bool) {
	d, err := s.Store.Device(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_device", "没有这台音箱")
		return
	}
	if on && (s.Stream == nil || s.Stream.MP3 == nil) {
		writeError(w, http.StatusConflict, "no_ffmpeg", "没找到 ffmpeg：换用带 ffmpeg 的镜像（madrays/trackferry:ffmpeg），或者放这首歌的在线 MP3 版本")
		return
	}
	if err := s.Store.SetDeviceMP3(r.Context(), d.DeviceID, on); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	d.MP3 = on
	s.Log.Info("音箱　改了无损转 MP3", "音箱", d.Name, "打开", on)
	writeJSON(w, http.StatusOK, s.deviceView(d))
}

func (s *Server) libraryFavorites(w http.ResponseWriter, r *http.Request) {
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	ts, n := s.Library.Favorites(from, 60)
	writeJSON(w, http.StatusOK, map[string]any{"tracks": ts, "total": n})
}

func (s *Server) libraryPlaylists(w http.ResponseWriter, r *http.Request) {
	out := []feiniu.Playlist{}
	linked := s.Router.Lists != nil && s.Router.Lists.Linked()
	if linked {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		all, err := s.Router.Lists.Playlists(ctx)
		if err != nil {
			writeError(w, http.StatusBadGateway, "feiniu", "没能读到飞牛音乐的歌单："+err.Error())
			return
		}
		out = append(out, all...)
	}
	// Playlists are 飞牛音乐's; a music folder has none, and the page says so rather than "make one".
	writeJSON(w, http.StatusOK, map[string]any{"playlists": out, "linked": linked})
}

// libraryPlaylistAdd puts a song into a playlist of 飞牛音乐.
func (s *Server) libraryPlaylistAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TrackID int64 `json:"track_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	changed, err := s.Router.PutIn(ctx, req.TrackID, r.PathValue("guid"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "feiniu", "没能加入歌单："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
}

func (s *Server) libraryPlaylist(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	name, ids, err := s.Router.Collect(ctx, r.PathValue("guid"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "feiniu", err.Error())
		return
	}
	ts := []store.TrackRow{}
	for _, id := range ids {
		if t, ok := s.Library.Track(id); ok {
			ts = append(ts, t)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "tracks": ts, "total": len(ids)})
}

func (s *Server) librarySearchOnline(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len([]rune(q)) > 100 {
		writeError(w, http.StatusBadRequest, "bad_text", "搜索词要在 1 到 100 字之间")
		return
	}
	songs, outs := s.Router.Online.Search(r.Context(), q)
	writeJSON(w, http.StatusOK, map[string]any{"songs": songs, "sources": outs})
}
