package takeover

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"trackferry/internal/intent"
	"trackferry/internal/live"
	"trackferry/internal/names"
	"trackferry/internal/routing"
	"trackferry/internal/store"
	"trackferry/internal/stream"
	"trackferry/internal/xiaomi"
)

// Decider routes a command (routing.Router).
type Decider interface {
	Decide(ctx context.Context, in intent.Intent, query string) *routing.Route
}

// Streams prepares audio for speakers (stream.Server).
type Streams interface {
	Song(rel, label string, d time.Duration) (*stream.Handle, error)
	Proxy(open stream.Opener, label string, d time.Duration) (*stream.Handle, error)
	SongMP3(ctx context.Context, rel, label string, d time.Duration) (*stream.Handle, error)
	WarmMP3(rel string)
}

// Starter runs takeovers (Engine).
type Starter interface {
	Start(ctx context.Context, req Request) (store.TakeoverRun, error)
	Release(deviceID string)
	Hear(deviceID string, h Heard)
	Quiet(deviceID string) func(back bool)
	Playing(deviceID string) bool
	Say(ctx context.Context, deviceID string, work func() string) error
}

// Dispatcher routes every recorded command and, on speakers set to takeover, plays the
// result. On watch-only speakers the route is recorded as a preview.
type Dispatcher struct {
	Engine  Starter
	Store   *store.Store
	Router  Decider
	Streams Streams
	// Favorite adds a song of the library to the favourites, or takes it out; optional.
	Favorite func(ctx context.Context, track int64, on bool) error
	// Next is the route of a later song of a list; optional.
	Next func(track int64, list string) (*routing.Route, bool)
	// Put adds a song of the library to a playlist, or takes it out; optional.
	Put  func(ctx context.Context, track int64, name string, on bool) (list string, changed bool, err error)
	Open func(*routing.Route) stream.Opener // online audio
	// Online gets an online song ready by its key; optional.
	Online func(ctx context.Context, key string) (*routing.Route, error)
	// Keep stores an online song that is being heard; optional.
	Keep    func(*routing.Route)
	Allowed func(deviceID string) bool // the speaker is released for takeover
	Log     *slog.Logger
	Live    *live.Hub // optional
	// Name of a speaker, for the web UI; optional.
	Name func(deviceID string) string
	// MP3 is whether lossless songs reach this speaker converted to MP3; optional.
	MP3 func(deviceID string) bool

	mu   sync.Mutex
	gens map[string]*generation
	last map[string]lastSong

	queues     map[string]*queue
	wished     int64    // names wishes
	startLocks sync.Map // device ID to mutex, for starting or advancing manual playback

	// Random draws songs of the library to go on with; optional.
	Random func(n int) []int64
}

type lastSong struct {
	routing.Previous
	track int64 // in the library; 0 = not there
	at    time.Time
}

// How long "another version" still points at a song.
const followFor = 15 * time.Minute

// Remember notes the song a speaker was asked for.
func (d *Dispatcher) Remember(dev, title, artist string, track int64) {
	if title == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		d.last = map[string]lastSong{}
	}
	p := routing.Previous{Title: title, Artist: artist}
	if old := d.last[dev]; names.Norm(old.Title) == names.Norm(title) {
		p.Tried = old.Tried
	}
	if artist != "" {
		p.Tried = append(slices.Clone(p.Tried), artist)
	}
	d.last[dev] = lastSong{p, track, time.Now()}
}

func (d *Dispatcher) previous(dev string) (routing.Previous, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.last[dev]
	p := l.Previous
	p.Track = l.track
	return p, ok && time.Since(l.at) < followFor
}

// Forget says the speaker is no longer on the song it was asked for.
func (d *Dispatcher) Forget(dev string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.last, dev)
}

// favorite does what was said about the song on the speaker, and says how it went.
// The second text is for saying aloud.
func (d *Dispatcher) favorite(ctx context.Context, dev string, on bool) (string, string) {
	d.mu.Lock()
	l, ok := d.last[dev]
	d.mu.Unlock()
	name := "《" + l.Title + "》"
	switch {
	case !ok:
		return "不知道说的是哪首歌", "没听出是哪首歌，没有收藏"
	case l.track == 0:
		return name + "不在曲库里，收藏不了", l.Title + "不在曲库里，没法收藏"
	case d.Favorite == nil:
		return "没有连接飞牛音乐", "还没连接飞牛音乐，没法收藏"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.Favorite(ctx, l.track, on); err != nil {
		d.Log.Warn("收藏　没有成功", "原因", err)
		if on {
			return name + "没能收藏：" + err.Error(), l.Title + "收藏失败了"
		}
		return name + "没能移出收藏：" + err.Error(), l.Title + "取消收藏失败了"
	}
	if on {
		return name + "已加入飞牛音乐的收藏", "已收藏" + l.Title
	}
	return name + "已移出飞牛音乐的收藏", "已取消收藏" + l.Title
}

func (d *Dispatcher) inLibrary(dev string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.last[dev]
	return ok && l.track != 0
}

// Follows reports that a speaker was asked for a song recently.
func (d *Dispatcher) Follows(dev string) bool { _, ok := d.previous(dev); return ok }

type generation struct {
	n      int
	cancel context.CancelFunc
}

// begin makes c the speaker's newest command, cancelling the routing of an older one.
func (d *Dispatcher) begin(ctx context.Context, dev string) (context.Context, func() bool, func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gens == nil {
		d.gens = map[string]*generation{}
	}
	g := d.gens[dev]
	if g == nil {
		g = &generation{}
		d.gens[dev] = g
	}
	if g.cancel != nil {
		g.cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	g.n++
	g.cancel = cancel
	n := g.n
	return ctx, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return g.n == n }, cancel
}

// TrackOf prepares a route's audio. ok is false when Xiao AI keeps the request.
func (d *Dispatcher) TrackOf(dev string, rt *routing.Route, tr *live.Trace) (Track, bool, error) {
	var h *stream.Handle
	var err error
	length := time.Duration(rt.DurationMS) * time.Millisecond
	switch {
	case rt.Match() != nil:
		if d.MP3 != nil && d.MP3(dev) {
			ctx, cancel := context.WithTimeout(context.Background(), mp3Wait)
			start := time.Now()
			h, err = d.Streams.SongMP3(ctx, rt.Path, rt.Label(), length)
			cancel()
			switch {
			case err == nil:
				took := time.Since(start)
				if took > time.Second {
					d.Log.Info("转码　无损转成 MP3", "歌", rt.Label(), "用时ms", took.Milliseconds())
					rt.Because("转码", "无损转成 320k MP3，等了 %.1f 秒", took.Seconds())
				} else {
					rt.Because("转码", "用转好的 320k MP3")
				}
			case errors.Is(err, stream.ErrNotLossless):
			default:
				d.Log.Warn("转码　没转成，改放原文件", "歌", rt.Label(), "原因", err)
				rt.Because("转码", "没转成，放原文件：%v", err)
			}
		}
		if h == nil || err != nil {
			h, err = d.Streams.Song(rt.Path, rt.Label(), length)
		}
		if h != nil && h.Lossless() && d.Store != nil {
			if dv, e := d.Store.Device(context.Background(), dev); e == nil && xiaomi.NoFLAC(dv.Hardware) {
				rt.Because("格式", "这个型号放不了无损原文件：不出声就在音箱页打开“无损转 MP3”，或换在线 MP3 版本")
			}
		}
	case rt.Found() != nil:
		if h, err = d.Streams.Proxy(d.Open(rt), rt.Label(), length); err == nil && d.Keep != nil {
			go d.keep(h, rt)
		}
	default:
		return Track{}, false, nil
	}
	if err != nil {
		return Track{}, false, err
	}
	Follow(h, tr)
	// No Duration: a song the user paused must not be cut off by the clock. Its end is the
	// speaker asking for the first byte again.
	return Track{URL: h.URL, Label: h.Label, Fetched: h.Fetched(), Restarted: h.Restarted(),
		Length: length, Lost: h.Lost, Resume: Resumer(h, tr), Seekable: h.Seekable()}, true, nil
}

// mp3Wait is the longest a speaker waits for a conversion before it gets the original file.
// The conversion goes on and is kept for the next time.
var mp3Wait = 90 * time.Second

// warm converts the song after the current one of q while it plays.
func (d *Dispatcher) warm(q *queue) {
	if d.MP3 == nil || !d.MP3(q.dev) {
		return
	}
	d.mu.Lock()
	var id int64
	if len(q.wishes) > 0 {
		id = q.wishes[0].Track
	} else if len(q.ids) > 1 {
		id = q.ids[(q.pos+1)%len(q.ids)]
	}
	d.mu.Unlock()
	if id <= 0 || d.Next == nil {
		return
	}
	if rt, ok := d.song(q, id); ok && rt.Match() != nil {
		d.Streams.WarmMP3(rt.Path)
	}
}

// How long a song must be wanted before it is kept. Tests shorten it.
var (
	keepFetch = time.Minute
	keepAfter = 20 * time.Second
)

// keep stores an online song once the speaker has been at it for a while.
func (d *Dispatcher) keep(h *stream.Handle, rt *routing.Route) {
	select {
	case <-h.Fetched():
	case <-time.After(keepFetch):
		return
	}
	time.Sleep(keepAfter)
	if !h.Lost() { // hung up early: not this song after all
		d.Keep(rt)
	}
}

// Resumer continues h's song from a point.
func Resumer(h *stream.Handle, tr *live.Trace) func(context.Context, time.Duration) (Track, time.Duration, error) {
	return func(ctx context.Context, at time.Duration) (Track, time.Duration, error) {
		n, start, err := h.Resume(ctx, at)
		if err != nil {
			return Track{}, 0, err
		}
		Follow(n, tr)
		return Track{URL: n.URL, Label: n.Label, Fetched: n.Fetched(), Restarted: n.Restarted(), Lost: n.Lost}, start, nil
	}
}

// Follow reports a stream's progress.
func Follow(h *stream.Handle, tr *live.Trace) {
	if tr != nil {
		h.Watch(func(s stream.Served) {
			tr.Audio(live.Audio{Bytes: s.Bytes, Total: s.Total, Requests: s.Requests, Open: s.Open})
		})
	}
}

// Halt ends the list on a speaker and whatever is being prepared for it.
func (d *Dispatcher) Halt(dev string) {
	d.Drop(dev)
	d.mu.Lock()
	defer d.mu.Unlock()
	if g := d.gens[dev]; g != nil {
		g.n++
		if g.cancel != nil {
			g.cancel()
		}
	}
}

// OnCommand is the observer callback.
func (d *Dispatcher) OnCommand(ctx context.Context, c store.Command, hardware, mode string) {
	ctx, current, done := d.begin(ctx, c.DeviceID)
	defer done()
	bg := context.WithoutCancel(ctx)
	take := mode == store.ModeTakeover && d.Allowed(c.DeviceID)
	// Why it sounds, or does not.
	act := func() string {
		switch {
		case take:
			return "这台音箱已接管，停下小爱，由我们放"
		case mode == store.ModeTakeover:
			return "这台音箱还没放行接管，只记录"
		}
		return "这台音箱设为观察，只记录"
	}
	stage := func(s, why string) {
		if mode == store.ModeTakeover {
			d.Store.SetCommandStage(bg, c.ID, s)
		}
		d.Log.Info("点歌　处理", "记录", c.ID, "模式", mode, "阶段", s, "说明", why)
	}
	in := intent.Intent{Kind: c.IntentKind, Title: c.IntentTitle, Artist: c.IntentArtist, Control: c.IntentControl}
	tr := d.Live.Begin(live.Flow{Kind: "command", DeviceID: c.DeviceID, Device: d.name(c.DeviceID), CommandID: c.ID, Said: c.Query,
		UpstreamMS: c.UpstreamMS, SeenMS: c.SeenMS, Intent: in})
	ctx = live.With(ctx, tr)
	if c.Stale {
		rt := &routing.Route{Decision: routing.Native, Layers: []routing.Layer{}, Preview: true, Reason: "读到得太晚，没有处理"}
		rt.Because("听成", "%s", routing.Heard(c.Query, in))
		rt.Because("没处理", "读到这句话时已经过了太久，再放会打断现在的播放")
		d.Store.SetCommandRoute(bg, c.ID, rt.JSON())
		tr.Route(*rt)
		tr.Done(rt.Reason)
		stage(store.StageSkipped, "record arrived too late")
		return
	}
	if in.Kind == intent.KindFavorite {
		rt := &routing.Route{Decision: routing.Native, Layers: []routing.Layer{}, Preview: !take, Reason: "只记录，没有收藏"}
		rt.Because("听成", "%s", routing.Heard(c.Query, in))
		sound := ""
		if take {
			// Xiao AI does not know what is playing and says so. Ours is the answer that counts;
			// over music of her own, hers is.
			work := func() (said string) {
				start := time.Now()
				switch in.Control {
				case intent.ListAdd, intent.ListRemove:
					rt.Reason, said = d.put(bg, c.DeviceID, in.Title, in.Control == intent.ListAdd)
				default:
					rt.Reason, said = d.favorite(bg, c.DeviceID, in.Control == "add")
				}
				rt.MS = time.Since(start).Milliseconds()
				return said
			}
			switch {
			case d.Engine.Playing(c.DeviceID):
				sound = "压住了小爱的回答，由我们报结果，歌接着放"
			case d.inLibrary(c.DeviceID):
				sound = "没有歌在放，没动小爱，只报了结果"
			}
			if sound == "" {
				work()
				sound = "放的不是我们推的歌，没动小爱"
			} else if err := d.Engine.Say(ctx, c.DeviceID, work); err != nil {
				sound = "结果没报出来：" + describe(err)
				if ctx.Err() != nil {
					sound = "又说了别的，没再报结果"
				}
			}
		}
		if !take {
			rt.Because("没收藏", "%s", act())
		} else {
			rt.Because("收藏", "%s", rt.Reason)
			rt.Because("出声", "%s", sound)
		}
		d.Store.SetCommandRoute(bg, c.ID, rt.JSON())
		tr.Route(*rt)
		tr.Done(rt.Reason)
		stage(store.StageNative, rt.Reason)
		return
	}
	if q := d.queued(c.DeviceID); take && q != nil && (in.Control == "next" || in.Control == "previous") {
		rt, req, ok := d.skip(q, c, hardware, tr)
		if !current() {
			tr.Done("同一台音箱有了更新的口令")
			stage(store.StageSkipped, "superseded while preparing")
			return
		}
		if ok && d.queued(c.DeviceID) == q {
			rt.Basis = nil
			rt.Because("听成", "%s", routing.Heard(c.Query, in))
			rt.Because("选它", "%s", skipped(q))
			rt.Because("出声", "%s", act())
			d.Remember(c.DeviceID, rt.Title, rt.Artist, rt.TrackID)
			d.Store.SetCommandRoute(bg, c.ID, rt.JSON())
			tr.Route(*rt)
			req.Ended = d.turn(q)
			if _, err := d.Engine.Start(ctx, req); err != nil {
				d.Drop(c.DeviceID)
				tr.Done("没能开始接管")
				stage(store.StageNative, "could not start takeover")
				return
			}
			stage(store.StageTakenOver, rt.Source)
			return
		}
	}
	if in.Kind == intent.KindControl {
		if in.Control == "next" || in.Control == "previous" {
			d.Forget(c.DeviceID)
			d.Drop(c.DeviceID)
		}
		if take {
			d.Engine.Release(c.DeviceID)
			d.Engine.Hear(c.DeviceID, Heard{Control: in.Control})
		}
		rt := &routing.Route{Decision: routing.Native, Layers: []routing.Layer{}, Preview: !take, Reason: "播放控制由小爱处理"}
		rt.Because("听成", "%s", routing.Heard(c.Query, in))
		rt.Because("交给小爱", "暂停、继续、停止、切歌由小爱自己做")
		d.Store.SetCommandRoute(bg, c.ID, rt.JSON())
		tr.Route(*rt)
		tr.Done(rt.Reason)
		stage(store.StageSkipped, "playback controls stay with Xiao AI")
		return
	}
	// Not a word of the answer, however long it takes to find the song.
	unquiet := func(bool) {}
	if take && in.Music() {
		unquiet = once(d.Engine.Quiet(c.DeviceID))
		defer unquiet(true)
	}
	// Whatever is asked for now, the list is over.
	list := d.unqueue(c.DeviceID)
	if p, ok := d.previous(c.DeviceID); ok {
		ctx = routing.After(ctx, p)
	}
	rt := d.Router.Decide(ctx, in, c.Query)
	rt.Preview = !take
	if rt.Decision != routing.Native {
		rt.Because("出声", "%s", act())
	} else {
		rt.Because("交给小爱", "%s", rt.Reason)
	}
	if !current() {
		rt.Reason, rt.Preview = "同一台音箱有了更新的口令", true
		rt.Because("没出声", "%s", rt.Reason)
		d.Store.SetCommandRoute(bg, c.ID, rt.JSON())
		tr.Route(*rt)
		tr.Done(rt.Reason)
		stage(store.StageSkipped, "superseded while routing")
		return
	}
	if rt.Decision != routing.Native {
		d.Remember(c.DeviceID, rt.Title, rt.Artist, rt.TrackID)
	} else if in.Kind == intent.KindSong {
		d.Remember(c.DeviceID, in.Title, in.Artist, 0)
	} else if in.Kind == intent.KindArtist || in.Kind == intent.KindRandom {
		d.Forget(c.DeviceID)
	}
	save := func() {
		if err := d.Store.SetCommandRoute(bg, c.ID, rt.JSON()); err != nil {
			d.Log.Warn("点歌　结果没有存上", "记录", c.ID, "原因", err)
		}
	}
	if !take {
		save()
		tr.Route(*rt)
		tr.Done("")
		stage(store.StageObserved, rt.Decision)
		return
	}
	t, ok, err := d.TrackOf(c.DeviceID, rt, tr)
	if err != nil {
		rt.Reason, rt.Decision = "准备音频失败："+err.Error(), routing.Native
		d.Log.Warn("点歌　准备音频失败", "记录", c.ID, "原因", err)
	}
	if err != nil {
		rt.Because("交给小爱", "%s", rt.Reason)
	}
	save()
	tr.Route(*rt)
	if !ok {
		tr.Done("")
		if in.Kind == intent.KindVersion && !slices.Contains(c.AnswerTypes, "AUDIO") {
			// Nothing to change to: the song goes on once Xiao AI has answered.
			if list != nil && d.Engine.Playing(c.DeviceID) {
				d.enqueue(list)
			}
			d.Engine.Hear(c.DeviceID, Heard{Answer: utf8.RuneCountInString(c.Answer), At: time.Now()})
		} else {
			d.Engine.Release(c.DeviceID)
			d.Engine.Hear(c.DeviceID, Heard{Audio: true})
		}
		stage(store.StageNative, rt.Reason)
		return
	}
	// Checked again right before acting: preparing audio takes time too.
	if !current() {
		tr.Done("同一台音箱有了更新的口令")
		stage(store.StageSkipped, "superseded while preparing")
		return
	}
	req := Request{DeviceID: c.DeviceID, Hardware: hardware, Kind: "command", CommandID: c.ID, UpstreamMS: c.UpstreamMS, Track: t, Trace: tr, Answer: utf8.RuneCountInString(c.Answer)}
	if ids := rt.Queue(); len(ids) > 0 && d.Next != nil {
		q := &queue{dev: c.DeviceID, hardware: hardware, list: rt.List, ids: ids, order: rt.Order()}
		if at := slices.Index(q.order, rt.TrackID); at >= 0 && d.PlayMode(bg, c.DeviceID) == ModeLoop {
			q.ids, q.pos = slices.Clone(q.order), at
		}
		req.Ended = d.follow(q)
	} else if more := rt.Ahead(); len(more) > 0 && d.Next != nil && d.Online != nil {
		req.Ended = d.follow(onlineQueue(c.DeviceID, hardware, rt, more))
	} else {
		req.Ended = d.After(c.DeviceID, hardware, rt.TrackID, rt, false)
	}
	if _, err := d.Engine.Start(ctx, req); err != nil {
		d.Drop(c.DeviceID)
		d.Log.Warn("接管　没有开始", "原因", err)
		tr.Done("没能开始接管")
		stage(store.StageNative, "could not start takeover")
		return
	}
	unquiet(false) // the run has it now
	stage(store.StageTakenOver, rt.Source)
}

// once lets only the first call through.
func once(f func(bool)) func(bool) {
	var o sync.Once
	return func(b bool) { o.Do(func() { f(b) }) }
}
