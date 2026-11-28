package takeover

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"trackferry/internal/library"
	"trackferry/internal/live"
	"trackferry/internal/online"
	"trackferry/internal/routing"
	"trackferry/internal/store"
)

// End is how a run ended.
type End int

const (
	EndOther    End = iota // stopped, replaced, or something else plays
	EndPlayed              // the song played through
	EndUnplayed            // the speaker did not play it
)

// queue is a list being played on a speaker, one song after another.
type queue struct {
	dev, hardware, list string
	ids                 []int64
	pos                 int
	turn                int    // which start is the current one
	fails               int    // songs in a row that did not play
	web                 bool   // sent from the page, so watching is enough
	by                  string // the browser that asked for it: 客厅 iPad; empty for Xiao AI
	until               time.Time

	order []int64        // the list as it was given
	first *routing.Route // song 0: one that is not in the library
	solo  bool           // a single song: nothing follows it on its own
	move  sync.Mutex     // one move at a time

	wishes    []Wish         // asked for by hand, before the list goes on
	aside     bool           // what plays is one of these; pos is where the list stands
	keepOrder bool           // a removed first song wraps to the next one without a shuffle
	outside   map[int64]Item // online songs handed over, under negative ids
}

// Wish is a song to play after the one that is on.
type Wish struct {
	ID     int64 // names it while it waits
	Track  int64 // 0: not in the library
	Title  string
	Artist string
	Route  func(ctx context.Context) (*routing.Route, error) // found anew: audio does not last
}

// maxWishes bounds what is asked for ahead.
const maxWishes = 50

// wishFor is how long finding a wish may take.
const wishFor = 20 * time.Second

// Want plays a song after the one that is on; false: nothing of ours is on.
func (d *Dispatcher) Want(dev string, w Wish) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.queues[dev]
	if q == nil {
		return false, nil
	}
	if len(q.wishes) >= maxWishes {
		return false, errors.New("排着的歌太多了")
	}
	d.wished++
	w.ID = d.wished
	q.wishes = append([]Wish{w}, q.wishes...)
	q.web = true
	return true, nil
}

// QueueTrack inserts a library song next or at the end. A song already there moves.
func (d *Dispatcher) QueueTrack(dev string, id int64, next bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.queues[dev]
	if q == nil {
		return false
	}
	q.web = true
	if q.ids[q.pos] == id {
		return true
	}
	if at := slices.Index(q.ids, id); at >= 0 {
		q.removeSlot(at)
	}
	if next {
		at := slices.Index(q.order, q.ids[q.pos])
		q.ids = slices.Insert(q.ids, q.pos+1, id)
		q.order = slices.Insert(q.order, at+1, id)
	} else {
		q.ids = append(q.ids, id)
		q.order = append(q.order, id)
	}
	q.solo = false
	return true
}

// wish takes the next wish that can be played.
func (d *Dispatcher) wish(ctx context.Context, q *queue) (*routing.Route, bool) {
	for {
		d.mu.Lock()
		if len(q.wishes) == 0 || d.queues[q.dev] != q {
			d.mu.Unlock()
			return nil, false
		}
		w := q.wishes[0]
		q.wishes = q.wishes[1:]
		d.mu.Unlock()
		c, cancel := context.WithTimeout(ctx, wishFor)
		rt, err := w.Route(c)
		cancel()
		if err == nil {
			return rt, true
		}
		d.Log.Warn("点播　这首跳过", "歌名", w.Title, "原因", err)
	}
}

// choose is what to play when somebody asks for another song.
func (d *Dispatcher) choose(ctx context.Context, q *queue, by int) (rt *routing.Route, ok bool) {
	d.mu.Lock()
	was, id := q.aside, q.ids[q.pos]
	d.mu.Unlock()
	if by > 0 {
		rt, ok = d.wish(ctx, q)
	}
	aside := ok
	if !ok && by < 0 && was { // back to where the list stands
		rt, ok = d.song(q, id)
	}
	if !ok {
		rt, ok = d.step(q, by)
	}
	if ok {
		d.mu.Lock()
		q.aside = aside
		d.mu.Unlock()
	}
	return rt, ok
}

// Modes of a speaker: what comes after a song.
const (
	ModeLoop    = "loop"    // the list in its order, over and over
	ModeShuffle = "shuffle" // in random order
	ModeOne     = "one"     // the same song again
)

const settingMode = "play.mode."

// PlayMode is how a speaker goes through a list.
func (d *Dispatcher) PlayMode(ctx context.Context, dev string) string {
	if m, _ := d.Store.Setting(ctx, settingMode+dev); m == ModeShuffle || m == ModeOne {
		return m
	}
	return ModeLoop
}

// SetPlayMode changes it. The song that plays goes on.
func (d *Dispatcher) SetPlayMode(ctx context.Context, dev, mode string) error {
	if err := d.Store.SetSetting(ctx, settingMode+dev, mode); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.queues[dev]
	if q == nil || mode == ModeOne {
		return nil
	}
	cur, at := q.ids[q.pos], slices.Index(q.order, q.ids[q.pos])
	q.ids, q.pos = slices.Clone(q.order), max(0, at)
	if mode == ModeShuffle {
		q.ids = slices.Delete(q.ids, q.pos, q.pos+1)
		rand.Shuffle(len(q.ids), func(i, j int) { q.ids[i], q.ids[j] = q.ids[j], q.ids[i] })
		q.ids, q.pos = slices.Insert(q.ids, 0, cur), 0
	}
	return nil
}

// Queue is what a page may know of the list on a speaker.
type Queue struct {
	List  string   `json:"list"` // empty: a single song
	At    int      `json:"at"`
	Total int      `json:"total"`
	Next  []string `json:"next"`         // asked for by hand, to come first
	By    string   `json:"by,omitempty"` // the browser that put it on
}

// Queue tells what plays on a speaker; nil when no song of ours does.
func (d *Dispatcher) Queue(dev string) *Queue {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.queues[dev]
	switch {
	case q == nil:
		return nil
	}
	out := &Queue{List: q.list, At: q.pos + 1, Total: len(q.ids), Next: []string{}}
	if q.solo {
		out = &Queue{At: 1, Total: 1, Next: []string{}}
	}
	for _, w := range q.wishes {
		out.Next = append(out.Next, w.Title)
	}
	out.By = q.by
	return out
}

// By is the browser that put on what plays on a speaker; "" when nothing of ours plays or
// Xiao AI was asked.
func (d *Dispatcher) By(dev string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if q := d.queues[dev]; q != nil {
		return q.by
	}
	return ""
}

// Tag marks what plays on a speaker as put on by a browser.
func (d *Dispatcher) Tag(dev, by string) {
	d.mu.Lock()
	if q := d.queues[dev]; q != nil {
		q.by = by
	}
	d.mu.Unlock()
	d.Live.MarkBy(dev, by)
}

// Why there is no other song.
var (
	ErrNoQueue = errors.New("没有正在放的歌")
	ErrNoSong  = errors.New("后面没有能放的歌")
)

// Skip plays the next song (by 1) or the one before (by -1), as a button asks for.
func (d *Dispatcher) Skip(ctx context.Context, dev string, by int) error {
	q := d.queued(dev)
	if q == nil {
		return ErrNoQueue
	}
	q.move.Lock()
	defer q.move.Unlock()
	if d.queued(dev) != q {
		return ErrNoQueue
	}
	rt, ok := d.choose(ctx, q, by)
	if !ok {
		return ErrNoSong
	}
	d.lasts(q) // somebody is listening
	return d.play(ctx, q, rt, false)
}

// Slot is a song of a list: where it stands, and which it is.
type Slot struct {
	At int   `json:"at"`
	ID int64 `json:"id"`
}

// Listing is all that comes after the song on a speaker, in order.
type Listing struct {
	List   string
	Total  int
	Aside  bool // what plays was asked for by hand
	Wishes []Wish
	Songs  []Slot
}

// Listing tells what comes; false when no song of ours is on.
func (d *Dispatcher) Listing(ctx context.Context, dev string) (Listing, bool) {
	loops := d.PlayMode(ctx, dev) == ModeLoop
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.queues[dev]
	if q == nil {
		return Listing{}, false
	}
	out := Listing{List: q.list, Total: len(q.ids), Aside: q.aside, Wishes: slices.Clone(q.wishes), Songs: []Slot{}}
	if q.solo {
		out.List, out.Total = "", 1
		return out, true
	}
	for i := q.pos + 1; i < len(q.ids); i++ {
		out.Songs = append(out.Songs, Slot{i, q.ids[i]})
	}
	if loops { // shuffled lists start over in another order
		for i := range q.pos {
			out.Songs = append(out.Songs, Slot{i, q.ids[i]})
		}
	}
	return out, true
}

// Pick names one of the songs of a Listing: a wish, or a slot.
type Pick struct {
	Wish int64 `json:"wish"`
	Slot
}

// ErrMoved is a song that does not come any more.
var ErrMoved = errors.New("列表已经变了，这首不在后面了")

// find says where a pick stands now. d.mu is held.
func (q *queue) find(p Pick) (at int, ok bool) {
	if p.Wish != 0 {
		at = slices.IndexFunc(q.wishes, func(w Wish) bool { return w.ID == p.Wish })
		return at, at >= 0
	}
	if p.At >= 0 && p.At < len(q.ids) && q.ids[p.At] == p.ID {
		return p.At, true
	}
	at = slices.Index(q.ids, p.ID)
	return at, at >= 0
}

// Jump plays one of the songs that come, and goes on from there.
func (d *Dispatcher) Jump(ctx context.Context, dev string, p Pick) error {
	q := d.queued(dev)
	if q == nil {
		return ErrNoQueue
	}
	q.move.Lock()
	defer q.move.Unlock()
	d.mu.Lock()
	if d.queues[dev] != q {
		d.mu.Unlock()
		return ErrNoQueue
	}
	at, ok := q.find(p)
	if !ok {
		d.mu.Unlock()
		return ErrMoved
	}
	var w Wish
	if p.Wish != 0 {
		w = q.wishes[at]
	}
	d.mu.Unlock()
	var rt *routing.Route
	if p.Wish != 0 {
		c, cancel := context.WithTimeout(ctx, wishFor)
		defer cancel()
		var err error
		if rt, err = w.Route(c); err != nil {
			return errors.New("这首放不了：" + err.Error())
		}
	} else if rt, ok = d.song(q, p.ID); !ok {
		return errors.New("这首歌暂时读不到")
	}
	d.mu.Lock()
	at, ok = q.find(p) // it may have moved meanwhile
	if ok {
		q.aside = p.Wish != 0
		if q.aside {
			q.wishes = slices.Delete(slices.Clone(q.wishes), at, at+1)
		} else {
			q.pos = at
		}
	}
	d.mu.Unlock()
	if !ok {
		return ErrMoved
	}
	d.lasts(q)
	return d.play(ctx, q, rt, false)
}

// Remove takes one of the songs that come off the list a speaker plays. Playlists and
// favourites keep it.
func (d *Dispatcher) Remove(dev string, p Pick) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.queues[dev]
	if q == nil {
		return ErrNoQueue
	}
	at, ok := q.find(p)
	switch {
	case !ok:
		return ErrMoved
	case p.Wish != 0:
		q.wishes = slices.Delete(slices.Clone(q.wishes), at, at+1)
		return nil
	case at == q.pos:
		return errors.New("这首正在放")
	}
	q.removeSlot(at)
	return nil
}

func (q *queue) removeSlot(at int) {
	id := q.ids[at]
	q.ids = slices.Delete(slices.Clone(q.ids), at, at+1)
	if at < q.pos {
		q.pos--
	} else if at == q.pos && q.aside {
		q.keepOrder = at == 0
		q.pos = (q.pos + len(q.ids) - 1) % len(q.ids)
	}
	if i := slices.Index(q.order, id); i >= 0 {
		q.order = slices.Delete(slices.Clone(q.order), i, i+1)
	}
}

// RemoveCurrent plays the next song and takes the one that was on off the list.
func (d *Dispatcher) RemoveCurrent(ctx context.Context, dev string) error {
	q := d.queued(dev)
	if q == nil {
		return ErrNoQueue
	}
	q.move.Lock()
	defer q.move.Unlock()
	d.mu.Lock()
	if d.queues[dev] != q {
		d.mu.Unlock()
		return ErrNoQueue
	}
	p, listed := Pick{Slot: Slot{q.pos, q.ids[q.pos]}}, !q.aside && !q.solo
	alone := listed && len(q.ids) == 1
	d.mu.Unlock()
	if alone {
		return errors.New("列表里只剩这一首了")
	}
	rt, ok := d.choose(ctx, q, 1)
	if !ok {
		return ErrNoSong
	}
	d.lasts(q)
	if err := d.play(ctx, q, rt, false); err != nil || !listed {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.queues[dev] == q {
		if at, ok := q.find(p); ok && len(q.ids) > 1 && (at != q.pos || q.aside) {
			q.removeSlot(at)
		}
	}
	return nil
}

// song is the route of one of a queue's songs.
func (d *Dispatcher) song(q *queue, id int64) (*routing.Route, bool) {
	if id < 0 {
		return d.outsider(q, id)
	}
	if id != 0 {
		return d.Next(id, q.list)
	}
	if q.first == nil {
		return nil, false
	}
	rt := *q.first
	return &rt, true
}

// more draws songs to come after a single one that was skipped.
func (d *Dispatcher) more(q *queue) {
	d.mu.Lock()
	dry := !q.web && q.solo && q.pos == len(q.ids)-1
	d.mu.Unlock()
	if !dry || d.Random == nil {
		return
	}
	ids := d.Random(afterwards)
	d.mu.Lock()
	defer d.mu.Unlock()
	cur := q.ids[q.pos]
	for _, id := range ids {
		if id != cur {
			q.ids, q.order = append(q.ids, id), append(q.order, id)
		}
	}
}

// lasts says until when songs follow on their own.
func (d *Dispatcher) lasts(q *queue) {
	t := time.Time{}
	if h := LoadPrefs(context.Background(), d.Store).Hours; h > 0 {
		t = time.Now().Add(time.Duration(h) * time.Hour)
	}
	d.mu.Lock()
	q.until = t
	d.mu.Unlock()
}

// maxFails ends a queue whose songs do not play.
const maxFails = 3

// Drop ends the list a speaker plays; the song on it goes on.
func (d *Dispatcher) Drop(dev string) { d.unqueue(dev) }

func (d *Dispatcher) unqueue(dev string) *queue {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.queues[dev]
	delete(d.queues, dev)
	return q
}

func (d *Dispatcher) enqueue(q *queue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.queues == nil {
		d.queues = map[string]*queue{}
	}
	d.queues[q.dev] = q
}

func (d *Dispatcher) queued(dev string) *queue {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.queues[dev]
}

// Playing settings.
const (
	SettingSingle = "play.single" // random songs follow what was not picked as a whole: on (default) | off
	SettingPool   = "play.pool"   // where they are drawn from: library (default) | favorites
	SettingHours  = "play.hours"  // how long songs follow on their own; 0: no limit
)

// DefaultHours ends a list nobody listens to any more.
const DefaultHours = 3

// Prefs is how songs follow one another, the same on every output. An artist's, an album's, the
// favourites' or a playlist's songs follow one of them; what follows anything else is set here.
type Prefs struct {
	Single bool   `json:"single"` // random songs follow a single song, the picks, "play some music"
	Pool   string `json:"pool"`   // library | favorites
	Hours  int    `json:"hours"`
}

// LoadPrefs reads the settings.
func LoadPrefs(ctx context.Context, st *store.Store) Prefs {
	get := func(k string) string { v, _ := st.Setting(ctx, k); return v }
	p := Prefs{Single: get(SettingSingle) != "off", Pool: "library", Hours: DefaultHours}
	if get(SettingPool) == "favorites" {
		p.Pool = "favorites"
	}
	if n, err := strconv.Atoi(get(SettingHours)); err == nil {
		p.Hours = n
	}
	return p
}

// SavePrefs stores the settings.
func SavePrefs(ctx context.Context, st *store.Store, p Prefs) error {
	single := "on"
	if !p.Single {
		single = "off"
	}
	return errors.Join(st.SetSetting(ctx, SettingSingle, single), st.SetSetting(ctx, SettingPool, p.Pool),
		st.SetSetting(ctx, SettingHours, strconv.Itoa(p.Hours)))
}

// Following draws what follows a song that was not picked as a whole, as the settings say: nothing
// when that is off. Every output asks this, so they all go on alike.
func Following(ctx context.Context, st *store.Store, lib *library.Library, n int) []int64 {
	p := LoadPrefs(ctx, st)
	switch {
	case !p.Single:
		return nil
	case p.Pool == "favorites":
		return lib.RandomFavoriteIDs(n)
	}
	return lib.RandomIDs(n)
}

// follow has ids play one after another on a speaker, the first of which is about to be
// started, and returns what the end of that one calls.
func (d *Dispatcher) follow(q *queue) func(End) {
	if q.order == nil {
		q.order = slices.Clone(q.ids)
	}
	d.lasts(q)
	d.enqueue(q)
	return d.turn(q)
}

// afterwards is how many random songs are drawn to follow a single one.
const afterwards = 500

// After looks after a single song: random songs follow it as the settings say, it repeats if the
// speaker is set to that, and it can be skipped. rt is for a song that is not in the library;
// its track is 0.
func (d *Dispatcher) After(dev, hardware string, track int64, rt *routing.Route, web bool) func(End) {
	if d.Next == nil {
		return nil
	}
	q := &queue{dev: dev, hardware: hardware, ids: []int64{track}, web: web, solo: true, first: rt}
	if d.Random != nil {
		for _, id := range d.Random(afterwards) {
			if id != track {
				q.ids = append(q.ids, id)
			}
		}
		if len(q.ids) > 1 {
			q.list, q.solo = "接着随机放", false
		}
	}
	return d.follow(q)
}

// PlayList plays a list on a speaker until something else is asked for.
func (d *Dispatcher) PlayList(ctx context.Context, dev store.Device, list string, ids []int64, start int64) (store.TakeoverRun, error) {
	order := slices.Clone(ids)
	ids = slices.Clone(ids)
	at := 0
	if start != 0 {
		at = slices.Index(ids, start)
		if at < 0 {
			return store.TakeoverRun{}, ErrMoved
		}
	}
	if d.PlayMode(ctx, dev.DeviceID) == ModeShuffle {
		if start != 0 {
			ids[0], ids[at] = ids[at], ids[0]
			rest := ids[1:]
			rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
			at = 0
		} else {
			rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		}
	}
	tries := min(10, len(ids))
	if start != 0 {
		tries = 1
	}
	for i, id := range ids[at : at+tries] {
		rt, ok := d.Next(id, list)
		if !ok {
			continue
		}
		q := &queue{dev: dev.DeviceID, hardware: dev.Hardware, list: list, ids: ids, pos: at + i, order: order, web: true}
		run, err := d.push(ctx, dev, rt, func() func(End) { return d.follow(q) })
		if err == nil {
			d.warm(q)
		}
		return run, err
	}
	return store.TakeoverRun{}, errors.New(list + "里的歌暂时读不到")
}

// Push plays one song that was picked by hand.
func (d *Dispatcher) Push(ctx context.Context, dev store.Device, rt *routing.Route) (store.TakeoverRun, error) {
	return d.push(ctx, dev, rt, func() func(End) { return d.After(dev.DeviceID, dev.Hardware, rt.TrackID, rt, true) })
}

func (d *Dispatcher) lockStart(dev string) func() {
	lock, _ := d.startLocks.LoadOrStore(dev, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (d *Dispatcher) push(ctx context.Context, dev store.Device, rt *routing.Route, ended func() func(End)) (store.TakeoverRun, error) {
	return d.pushAt(ctx, dev, rt, ended, 0, false)
}

func (d *Dispatcher) pushAt(ctx context.Context, dev store.Device, rt *routing.Route, ended func() func(End), at time.Duration, handoff bool) (store.TakeoverRun, error) {
	if err := ctx.Err(); err != nil {
		return store.TakeoverRun{}, err
	}
	tr := d.Live.Begin(live.Flow{Kind: "push", DeviceID: dev.DeviceID, Device: d.name(dev.DeviceID), Said: rt.Label()})
	t, ok, err := d.TrackOf(dev.DeviceID, rt, tr)
	if err == nil && !ok {
		err = errors.New("没有可放的音频")
	}
	if err != nil {
		tr.Done("准备音频失败")
		return store.TakeoverRun{}, err
	}
	var actual time.Duration
	if at > 0 {
		if !t.Seekable || t.Resume == nil {
			tr.Done("歌曲不能定位")
			return store.TakeoverRun{}, ErrCannotSeek
		}
		var next Track
		next, actual, err = t.Resume(ctx, at)
		if err != nil {
			tr.Done("准备接续音频失败")
			return store.TakeoverRun{}, err
		}
		// The cut stream provides the initial bytes; further seeks still address the original song.
		t.URL, t.Fetched, t.Restarted, t.Lost = next.URL, next.Fetched, next.Restarted, next.Lost
	}
	unlock := d.lockStart(dev.DeviceID)
	defer unlock()
	if err := ctx.Err(); err != nil {
		tr.Done("有了更新的播放请求")
		return store.TakeoverRun{}, err
	}
	d.Halt(dev.DeviceID) // prepared successfully; it wins over the prior queue
	d.Remember(dev.DeviceID, rt.Title, rt.Artist, rt.TrackID)
	song := &store.Song{TrackID: rt.TrackID, Title: rt.Title, Artist: rt.Artist, Album: rt.Album, Cover: rt.Cover, DurationMS: rt.DurationMS, List: rt.List, Online: onlineKey(rt)}
	run, err := d.Engine.Start(ctx, Request{DeviceID: dev.DeviceID, Hardware: dev.Hardware, Kind: "push", Track: t, StartAt: actual, Song: song, Trace: tr, Ended: ended(), Handoff: handoff})
	if err != nil {
		d.Drop(dev.DeviceID)
		tr.Done("没能开始")
	}
	return run, err
}

// turn makes the next start the current one and returns what its end calls.
func (d *Dispatcher) turn(q *queue) func(End) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q.turn++
	n := q.turn
	return func(how End) {
		d.mu.Lock()
		on := d.queues[q.dev] == q && q.turn == n
		if on {
			switch how {
			case EndPlayed:
				q.fails = 0
			case EndUnplayed:
				q.fails++
			}
			if on = how != EndOther && q.fails < maxFails; !on {
				delete(d.queues, q.dev)
			}
		}
		d.mu.Unlock()
		if on {
			d.advance(q, n)
		}
	}
}

// step moves the queue and returns a song that can be read now. The list starts over, in a
// new order, when it is through.
func (d *Dispatcher) step(q *queue, by int) (*routing.Route, bool) {
	back := by < 0
	if !back {
		d.more(q)
	}
	shuffled := d.PlayMode(context.Background(), q.dev) == ModeShuffle
	d.mu.Lock()
	was, n := q.ids[q.pos], len(q.ids)
	d.mu.Unlock()
	for i := range min(40, 2*n) {
		d.mu.Lock()
		q.pos += by
		if q.pos < 0 {
			q.pos = 0
			by = 1 // if the first song cannot be read, the one after it
		}
		if q.pos >= len(q.ids) {
			if shuffled && !q.keepOrder {
				rand.Shuffle(len(q.ids), func(i, j int) { q.ids[i], q.ids[j] = q.ids[j], q.ids[i] })
			}
			q.keepOrder = false
			q.pos = 0
		}
		id := q.ids[q.pos]
		d.mu.Unlock()
		if id == was && !back && i < n-1 {
			continue // not twice in a row, unless it is all there is
		}
		if rt, ok := d.song(q, id); ok {
			return rt, true
		}
	}
	return nil, false
}

// advance plays the next song of a queue after turn n ended.
func (d *Dispatcher) advance(q *queue, n int) {
	ctx := context.Background()
	q.move.Lock()
	defer q.move.Unlock()
	d.mu.Lock()
	moved := d.queues[q.dev] != q || q.turn != n
	id, until, fails, web := q.ids[q.pos], q.until, q.fails, q.web
	d.mu.Unlock()
	if moved {
		return
	}
	end := func(why string) {
		d.finished(q, n)
		d.Log.Info("连播　结束", "说明", why)
	}
	dev, err := d.Store.Device(ctx, q.dev)
	if err != nil || dev.Mode == store.ModeOff || !dev.InAccount || !web && (dev.Mode != store.ModeTakeover || !d.Allowed(q.dev)) {
		end("音箱不再接管")
		return
	}
	if !until.IsZero() && time.Now().After(until) {
		end("放够了设定的最长时间")
		return
	}
	var rt *routing.Route
	ok := false
	if rt, ok = d.wish(ctx, q); ok {
		d.mu.Lock()
		q.aside = true
		d.mu.Unlock()
		if err := d.play(ctx, q, rt, true); err != nil {
			end(err.Error())
		}
		return
	}
	d.mu.Lock()
	q.aside = false
	solo, count := q.solo, len(q.ids)
	d.mu.Unlock()
	if d.PlayMode(ctx, q.dev) == ModeOne && (solo || fails == 0) {
		rt, ok = d.song(q, id)
	}
	if !ok && (solo || web && count == 1) {
		d.finished(q, n)
		return
	}
	if !ok {
		rt, ok = d.step(q, 1)
	}
	if !ok {
		end("后面的歌暂时读不到")
		return
	}
	if err := d.play(ctx, q, rt, true); err != nil {
		end(err.Error())
	}
}

// finished ends a queue, unless it has moved on.
func (d *Dispatcher) finished(q *queue, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.queues[q.dev] == q && q.turn == n {
		delete(d.queues, q.dev)
	}
}

// play starts a song of a queue. over: the one before it has ended, so nothing needs silencing.
func (d *Dispatcher) play(ctx context.Context, q *queue, rt *routing.Route, over bool) error {
	d.mu.Lock()
	by := q.by
	d.mu.Unlock()
	tr := d.Live.Begin(live.Flow{Kind: "push", DeviceID: q.dev, Device: d.name(q.dev), Said: rt.Label(), By: by})
	t, _, err := d.TrackOf(q.dev, rt, tr)
	if err != nil {
		tr.Done("准备音频失败")
		return errors.New("准备音频失败：" + err.Error())
	}
	unlock := d.lockStart(q.dev)
	defer unlock()
	if d.queued(q.dev) != q {
		tr.Done("有了更新的口令")
		return ErrNoQueue
	}
	ended := d.turn(q)
	list := q.list
	d.mu.Lock()
	if q.aside {
		list = ""
	}
	d.mu.Unlock()
	d.Remember(q.dev, rt.Title, rt.Artist, rt.TrackID)
	song := &store.Song{TrackID: rt.TrackID, Title: rt.Title, Artist: rt.Artist, Album: rt.Album, Cover: rt.Cover, DurationMS: rt.DurationMS, List: list, Online: onlineKey(rt)}
	if _, err := d.Engine.Start(ctx, Request{DeviceID: q.dev, Hardware: q.hardware, Kind: "push", Track: t, Song: song, Trace: tr, Follows: over, Ended: ended}); err != nil {
		d.mu.Lock()
		if d.queues[q.dev] == q {
			delete(d.queues, q.dev)
		}
		d.mu.Unlock()
		tr.Done("没能开始")
		return errors.New("没能开始：" + err.Error())
	}
	d.warm(q)
	return nil
}

func (d *Dispatcher) name(dev string) string {
	if d.Name == nil {
		return ""
	}
	return d.Name(dev)
}

// skip answers "下一首" and "上一首" while a list plays.
func (d *Dispatcher) skip(q *queue, c store.Command, hardware string, tr *live.Trace) (*routing.Route, Request, bool) {
	by := 1
	if c.IntentControl == "previous" {
		by = -1
	}
	start := time.Now()
	q.move.Lock()
	defer q.move.Unlock()
	rt, ok := d.choose(context.Background(), q, by)
	if !ok {
		return nil, Request{}, false
	}
	t, _, err := d.TrackOf(q.dev, rt, tr)
	if err != nil {
		return nil, Request{}, false
	}
	rt.MS = time.Since(start).Milliseconds()
	return rt, Request{DeviceID: q.dev, Hardware: hardware, Kind: "command", CommandID: c.ID, UpstreamMS: c.UpstreamMS, Track: t, Trace: tr}, true
}

// put adds the song on the speaker to a playlist, or takes it out, and says how it went.
func (d *Dispatcher) put(ctx context.Context, dev, name string, on bool) (string, string) {
	d.mu.Lock()
	l, ok := d.last[dev]
	d.mu.Unlock()
	song := "《" + l.Title + "》"
	switch {
	case !ok:
		return "不知道说的是哪首歌", "没听出是哪首歌"
	case l.track == 0:
		return song + "不在曲库里，加不进歌单", l.Title + "不在曲库里，没法加进歌单"
	case d.Put == nil:
		return "没有连接飞牛音乐", "还没连接飞牛音乐"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	list, changed, err := d.Put(ctx, l.track, name, on)
	switch {
	case err != nil:
		d.Log.Warn("歌单　没有成功", "原因", err)
		said := l.Title + "没能加进歌单"
		if !on {
			said = l.Title + "没能移出歌单"
		}
		if list == "" { // which playlist: worth hearing, and it holds no addresses
			said = strings.TrimSpace(sayable(err))
		}
		return song + "：" + err.Error(), said
	case on && changed:
		return song + "已加入歌单“" + list + "”", "已加入" + list + "歌单"
	case on:
		return song + "已经在歌单“" + list + "”里", "已经在" + list + "歌单里了"
	case changed:
		return song + "已移出歌单“" + list + "”", "已移出" + list + "歌单"
	}
	return song + "不在歌单“" + list + "”里", "它不在" + list + "歌单里"
}

// sayable shortens an error for the speaker.
func sayable(err error) string {
	s, _, cut := strings.Cut(err.Error(), "：")
	if cut && !strings.HasPrefix(s, "有几个") {
		return s
	}
	return err.Error()
}

// skipped says why a song came after "下一首" or "上一首".
func skipped(q *queue) string {
	if q.solo {
		return "放的是单曲，切到别的歌"
	}
	return q.list + "正在连播"
}

// Snapshot captures the currently sounding local recording and the future queue.
func (d *Dispatcher) Snapshot(dev string, current Item) ([]Item, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.queues[dev]
	if q == nil || q.pos >= len(q.ids) {
		return []Item{current}, 0
	}
	item := func(id int64) (Item, bool) {
		if id > 0 {
			return Item{ID: id}, true
		}
		it, ok := q.outside[id]
		return it, ok
	}
	var items []Item
	for _, id := range q.ids[:q.pos] {
		if it, ok := item(id); ok {
			items = append(items, it)
		}
	}
	at := len(items)
	items = append(items, current)
	for _, w := range q.wishes {
		if w.Track > 0 {
			items = append(items, Item{ID: w.Track})
		}
	}
	for _, id := range q.ids[q.pos+1:] {
		if it, ok := item(id); ok {
			items = append(items, it)
		}
	}
	return items, at
}

// Item is one song handed over: a song of the library (ID) or an online one (Online, its key).
type Item struct {
	ID     int64
	Online string
	Title  string
	Artist string
}

// onlineKey is the key of an online song that is not in the library, or "".
func onlineKey(rt *routing.Route) string {
	if f := rt.Found(); f != nil && rt.TrackID == 0 {
		return f.Candidate.Key
	}
	return ""
}

// onlineQueue plays on the online songs a route found after it, round and round. The one that
// plays first is a song of the queue too, so it is found anew when the list comes back to it.
func onlineQueue(dev, hardware string, rt *routing.Route, more []online.Candidate) *queue {
	q := &queue{dev: dev, hardware: hardware, list: rt.List, outside: map[int64]Item{}}
	add := func(key, title, artist string) {
		id := -int64(len(q.ids) + 1)
		q.ids = append(q.ids, id)
		q.outside[id] = Item{Online: key, Title: title, Artist: artist}
	}
	add(onlineKey(rt), rt.Title, rt.Artist)
	for _, c := range more {
		add(c.Key, c.Title, c.Artist)
	}
	return q
}

// outsider finds an online song of a queue anew: its audio does not last.
func (d *Dispatcher) outsider(q *queue, id int64) (*routing.Route, bool) {
	d.mu.Lock()
	it, ok := q.outside[id]
	d.mu.Unlock()
	if !ok || d.Online == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), wishFor)
	defer cancel()
	rt, err := d.Online(ctx, it.Online)
	if err != nil {
		d.Log.Warn("接力　在线歌曲取不到", "歌名", it.Title, "原因", err)
		return nil, false
	}
	return rt, true
}

// Receive keeps the incoming order and starts the selected occurrence at a measured offset.
func (d *Dispatcher) Receive(ctx context.Context, dev store.Device, items []Item, index int, at time.Duration) (store.TakeoverRun, error) {
	const list = "接力播放"
	if index < 0 || index >= len(items) {
		return store.TakeoverRun{}, ErrMoved
	}
	q := &queue{dev: dev.DeviceID, hardware: dev.Hardware, list: list, ids: make([]int64, len(items)), pos: index, web: true, solo: len(items) == 1}
	for i, it := range items {
		if it.ID > 0 {
			q.ids[i] = it.ID
			continue
		}
		if q.outside == nil {
			q.outside = map[int64]Item{}
		}
		q.ids[i] = -int64(i + 1)
		q.outside[q.ids[i]] = it
	}
	if len(items) == 1 && d.Random != nil { // a song alone goes on as anywhere else
		for _, id := range d.Random(afterwards) {
			if id != q.ids[0] {
				q.ids = append(q.ids, id)
			}
		}
		if len(q.ids) > 1 {
			q.list, q.solo = "接着随机放", false
		}
	}
	var rt *routing.Route
	if id := q.ids[index]; id > 0 {
		var ok bool
		if rt, ok = d.Next(id, list); !ok {
			return store.TakeoverRun{}, ErrNoSong
		}
	} else {
		if d.Online == nil {
			return store.TakeoverRun{}, ErrNoSong
		}
		var err error
		if rt, err = d.Online(ctx, items[index].Online); err != nil {
			return store.TakeoverRun{}, err
		}
	}
	run, err := d.pushAt(ctx, dev, rt, func() func(End) { return d.follow(q) }, at, true)
	if err == nil {
		d.warm(q)
	}
	return run, err
}
