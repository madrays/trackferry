// Package observe polls the Xiao AI conversation records of enabled speakers and records
// the music commands it recognises. It never controls a speaker itself; a registered
// OnCommand callback (the takeover dispatcher) decides what to do with each command.
//
// Each speaker gets a watermark taken from Xiaomi's own record timestamps. The first
// successful read after start (or after the speaker is enabled) only sets the watermark,
// so commands spoken before TrackFerry was watching are never replayed. A failed read
// never moves the watermark: "could not read" is not "nothing new".
package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"trackferry/internal/intent"
	"trackferry/internal/live"
	"trackferry/internal/logx"
	"trackferry/internal/store"
	"trackferry/internal/xiaomi"
)

// Source reads conversation records (the xiaomi.Manager).
type Source interface {
	Conversations(ctx context.Context, deviceID, hardware string, limit int) ([]xiaomi.Record, error)
	Status() xiaomi.AccountStatus
}

// Device watch states.
const (
	StateStarting    = "starting"    // waiting for the first read to set the watermark
	StateListening   = "listening"   // watermark set; new commands are recorded
	StateError       = "error"       // reads are failing; retrying with backoff
	StatePaused      = "paused"      // no usable Xiaomi login
	StateUnsupported = "unsupported" // model whose records need another API
)

// staleAfter marks records that became visible long after Xiaomi recognised them. They are
// recorded but must never be acted on once takeover exists.
const staleAfter = 2 * time.Minute

// modelsWithoutRecords read conversations through a different (ubus) API, see xiaomusic
// GET_ASK_BY_MINA; not supported yet.
var modelsWithoutRecords = map[string]bool{"M01": true}

// DeviceStatus is one watched speaker.
type DeviceStatus struct {
	DeviceID   string    `json:"device_id"`
	Name       string    `json:"name"`
	State      string    `json:"state"`
	LastPollAt time.Time `json:"last_poll_at,omitzero"`
	LastOKAt   time.Time `json:"last_ok_at,omitzero"`
	Error      string    `json:"error,omitempty"`
	ErrorKind  string    `json:"error_kind,omitempty"`
	Recorded   int       `json:"recorded"` // music commands recorded since the watch started
	Ignored    int       `json:"ignored"`  // other utterances, counted but not stored
	Since      time.Time `json:"since"`
	Idle       bool      `json:"idle,omitempty"` // read slower: nothing heard for a while
}

// Status is the observer as a whole.
type Status struct {
	Paused     string         `json:"paused,omitempty"` // unlinked | relink_required
	IntervalMS int64          `json:"interval_ms"`
	Devices    []DeviceStatus `json:"devices"`
}

type watch struct {
	status    DeviceStatus
	hardware  string
	mode      string
	watermark int64
	baseline  bool
	failures  int
	next      time.Time
	inflight  bool
	heard     time.Time // when it last heard anything, music or not
}

// Observer is safe for concurrent use.
type Observer struct {
	src      Source
	st       *store.Store
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time

	// Live, when set, hears about every look at a speaker.
	Live *live.Hub

	// OnOther, when set, hears that someone said something that is not a request. Its words
	// are used at once and never kept.
	OnOther func(deviceID string, r xiaomi.Record)

	// Follows reports that a song was asked for a moment ago, so "换个版本" means something.
	Follows func(deviceID string) bool

	onCommand func(ctx context.Context, c store.Command, hardware, mode string)

	mu      sync.Mutex
	watches map[string]*watch
	paused  string
	prefs   Prefs     // off, idle and sleep; the interval is kept apart for tests
	refused int       // reads Xiaomi refused in a row, any speaker
	cool    time.Time // no speaker of the account is read before this
	wg      sync.WaitGroup
}

// Listening prefs, kept server side as one JSON setting.
const (
	DefaultIntervalMS = 2000
	MinIntervalMS     = 1000
	MaxIntervalMS     = 10000
	DefaultIdleMS     = 6000
	MaxIdleMS         = 30000

	// HotFor is how long a speaker stays read at the full rate after it last heard anything,
	// unless set otherwise (Prefs.QuietMin).
	HotFor      = 10 * time.Minute
	MaxQuietMin = 60

	settingListening = "listening"
)

// Prefs is how speakers are listened to.
type Prefs struct {
	Listen     bool   `json:"listen"`      // off: speakers only play what is sent to them
	IntervalMS int    `json:"interval_ms"` // how often a speaker in takeover is read; watched ones half as often
	Idle       bool   `json:"idle"`        // read slower after HotFor without a word
	IdleMS     int    `json:"idle_ms"`     // how often then
	QuietMin   int    `json:"quiet_min"`   // minutes without a word before it is read slower
	Sleep      bool   `json:"sleep"`       // no reads at all between SleepFrom and SleepTo
	SleepFrom  string `json:"sleep_from"`  // "23:00", local time
	SleepTo    string `json:"sleep_to"`    // "07:00"; earlier than SleepFrom: the window spans midnight
	Together   bool   `json:"together"`    // a refusal makes every speaker of the account wait
}

// DefaultPrefs is what an unset or unreadable setting means.
func DefaultPrefs() Prefs {
	return Prefs{Listen: true, IntervalMS: DefaultIntervalMS, Idle: true, IdleMS: DefaultIdleMS, QuietMin: int(HotFor / time.Minute), SleepFrom: "01:00", SleepTo: "07:00", Together: true}
}

// quietFor is how long a speaker stays read at the full rate after it last heard anything.
func (p Prefs) quietFor() time.Duration {
	if p.QuietMin <= 0 {
		return HotFor
	}
	return time.Duration(p.QuietMin) * time.Minute
}

// Check reports what is wrong with p, in words for the page; "" when nothing is.
func (p Prefs) Check() string {
	switch {
	case p.IntervalMS < MinIntervalMS || p.IntervalMS > MaxIntervalMS:
		return fmt.Sprintf("间隔要在 %d 到 %d 秒之间", MinIntervalMS/1000, MaxIntervalMS/1000)
	case p.IdleMS < p.IntervalMS || p.IdleMS > MaxIdleMS:
		return fmt.Sprintf("空闲时的间隔要在平时的间隔到 %d 秒之间", MaxIdleMS/1000)
	case p.QuietMin < 1 || p.QuietMin > MaxQuietMin:
		return fmt.Sprintf("多久没人说话算安静要在 1 到 %d 分钟之间", MaxQuietMin)
	case clockOf(p.SleepFrom) < 0 || clockOf(p.SleepTo) < 0:
		return "睡眠时段要写成 23:00 这样"
	case p.SleepFrom == p.SleepTo:
		return "睡眠时段的开始和结束不能一样"
	}
	return ""
}

// clockOf is "HH:MM" as minutes into the day, or -1.
func clockOf(v string) int {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return -1
	}
	return t.Hour()*60 + t.Minute()
}

// asleep is whether t falls in the sleep window of p.
func (p Prefs) asleep(t time.Time) bool {
	if !p.Sleep {
		return false
	}
	from, to, m := clockOf(p.SleepFrom), clockOf(p.SleepTo), t.Hour()*60+t.Minute()
	if from < 0 || to < 0 || from == to {
		return false
	}
	if from < to {
		return m >= from && m < to
	}
	return m >= from || m < to
}

// LoadPrefs reads the listening prefs; what is missing or wrong is the default.
func LoadPrefs(ctx context.Context, st *store.Store) Prefs {
	p := DefaultPrefs()
	if v, _ := st.Setting(ctx, settingListening); v != "" {
		q := DefaultPrefs()
		if json.Unmarshal([]byte(v), &q) == nil && q.Check() == "" {
			p = q
		}
	}
	return p
}

// SavePrefs stores p, which the caller has checked.
func SavePrefs(ctx context.Context, st *store.Store, p Prefs) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, settingListening, string(b))
}

// Apply takes new prefs at once: the next look at the speakers follows them.
func (o *Observer) Apply(p Prefs) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if p.IntervalMS >= MinIntervalMS && p.IntervalMS <= MaxIntervalMS {
		o.interval = time.Duration(p.IntervalMS) * time.Millisecond
	}
	o.prefs = p
}

func (o *Observer) every() time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.interval
}

// New builds an Observer polling every interval.
func New(src Source, st *store.Store, log *slog.Logger, interval time.Duration) *Observer {
	return &Observer{src: src, st: st, log: log, interval: interval, now: time.Now, watches: map[string]*watch{}, prefs: Prefs{Listen: true, Together: true}}
}

// OnCommand registers f to be called (in its own goroutine) for every newly recorded
// command, with the speaker's hardware model and mode. Call before Run.
func (o *Observer) OnCommand(f func(ctx context.Context, c store.Command, hardware, mode string)) {
	o.onCommand = f
}

// Status reports every watched speaker.
func (o *Observer) Status() Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := Status{Paused: o.paused, IntervalMS: o.interval.Milliseconds(), Devices: []DeviceStatus{}}
	for _, w := range o.watches {
		out.Devices = append(out.Devices, w.status)
	}
	sort.Slice(out.Devices, func(i, j int) bool { return out.Devices[i].Name < out.Devices[j].Name })
	return out
}

// maxOffline bounds the wait while Xiaomi cannot be reached: once the line is back, the next
// request should be heard at once.
const maxOffline = 5 * time.Second

// pollTimeout bounds one read; a record read later than that is hardly worth waiting for.
const pollTimeout = 6 * time.Second

// maxRefused bounds the wait after Xiaomi refuses reads.
const maxRefused = 5 * time.Minute

// Run polls until ctx ends. Speakers in takeover are read every interval, the others every
// second one; a changed interval takes effect at the next tick.
func (o *Observer) Run(ctx context.Context) {
	t := time.NewTimer(o.every())
	defer t.Stop()
	var pruned time.Time
	for n := 0; ; n++ {
		if now := o.now(); now.Sub(pruned) > 24*time.Hour {
			pruned = now
			if n, err := o.st.Prune(ctx, now.AddDate(0, 0, -180)); err != nil {
				o.log.Warn("记录　清理没有完成", "原因", err)
			} else if n > 0 {
				o.log.Info("记录　清理过期", "条数", n)
			}
		}
		o.tick(ctx, n%2 == 1)
		select {
		case <-ctx.Done():
			o.wg.Wait()
			return
		case <-t.C:
			t.Reset(o.every())
		}
	}
}

// Tick starts a read for every speaker that is due. Exposed for tests.
func (o *Observer) Tick(ctx context.Context) { o.tick(ctx, false) }

// tick between passes over speakers that are only watched: those whose answer we have to
// interrupt are read twice as often, unless reading them fails. While Xiaomi refuses reads,
// no speaker of the account is read: asking again with the others only makes it worse.
func (o *Observer) tick(ctx context.Context, between bool) {
	devs, err := o.st.Devices(ctx)
	if err != nil {
		o.log.Warn("听歌　读不到音箱列表", "原因", err)
		return
	}
	acct := o.src.Status()
	paused := ""
	switch {
	case acct.RelinkRequired:
		paused = xiaomi.SyncRelinkRequired
	case !acct.Linked:
		paused = "unlinked"
	}
	now := o.now()
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case !o.prefs.Listen:
		paused = "off" // switched off in the settings: speakers only play what is sent to them
	case o.prefs.asleep(now):
		paused = "sleep"
	}
	o.paused = paused
	active := map[string]bool{}
	for _, d := range devs {
		if d.Mode == store.ModeOff || !d.InAccount {
			continue
		}
		active[d.DeviceID] = true
		w := o.watches[d.DeviceID]
		if w == nil {
			w = &watch{status: DeviceStatus{DeviceID: d.DeviceID, State: StateStarting, Since: now}}
			o.watches[d.DeviceID] = w
		}
		w.status.Name = firstNonEmpty(d.Alias, d.Name, d.DeviceID)
		w.hardware, w.mode = d.Hardware, d.Mode
		if modelsWithoutRecords[strings.ToUpper(d.Hardware)] {
			w.status.State, w.status.Error = StateUnsupported, "这个型号的对话记录要走另一种接口，暂不支持"
			continue
		}
		if paused != "" {
			// A watch that resumes after a pause starts from a fresh watermark.
			w.status.State, w.baseline = StatePaused, false
			continue
		}
		if w.inflight || now.Before(w.next) || now.Before(o.cool) || between && (w.mode != store.ModeTakeover || w.failures > 0) {
			continue
		}
		w.inflight = true
		o.wg.Add(1)
		go o.poll(ctx, d.DeviceID, w)
	}
	// Speakers switched off (or gone) lose their watch; re-enabling sets a new watermark.
	for id := range o.watches {
		if !active[id] {
			delete(o.watches, id)
		}
	}
}

func (o *Observer) poll(ctx context.Context, id string, w *watch) {
	defer o.wg.Done()
	defer logx.Recover(o.log, "听歌")
	defer func() { // also after a fault: the speaker must be listened to again
		o.mu.Lock()
		w.inflight = false
		o.mu.Unlock()
	}()
	o.mu.Lock()
	hardware, every := w.hardware, o.interval
	if w.mode != store.ModeTakeover || w.failures > 0 {
		every *= 2
	}
	o.mu.Unlock()
	started := o.now()
	rctx, cancel := context.WithTimeout(ctx, pollTimeout)
	recs, err := o.src.Conversations(rctx, id, hardware, 5)
	cancel()

	o.mu.Lock()
	defer o.mu.Unlock()
	w.status.LastPollAt = started
	took := o.now().Sub(started).Milliseconds()
	defer func() {
		if o.watches[id] == w {
			o.Live.Pulse(live.Pulse{DeviceID: id, Name: w.status.Name, State: w.status.State, OK: err == nil, MS: took,
				Ignored: w.status.Ignored, Recorded: w.status.Recorded, Every: every.Milliseconds()})
		}
	}()
	if o.watches[id] != w {
		return // switched off meanwhile
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.failures++
		backoff := o.interval << min(w.failures, 5)
		if k := xiaomi.KindOf(err); k == xiaomi.KindNetwork {
			backoff = min(backoff, maxOffline)
		} else if w.failures > 3 { // refused: leave Xiaomi alone for a while
			backoff = min(o.interval<<min(w.failures, 10), maxRefused)
		}
		w.next = started.Add(backoff)
		w.status.Error, w.status.ErrorKind = explain(err), xiaomi.KindOf(err)
		if w.failures >= 2 {
			w.status.State = StateError
		}
		if refused(err) && o.prefs.Together {
			// Xiaomi limits the account, not the speaker: every speaker waits together, longer
			// each time, at most maxRefused. The speakers still say that reading fails.
			o.refused++
			wait := min(o.interval<<min(o.refused, 10), maxRefused)
			if until := started.Add(wait); until.After(o.cool) {
				o.cool = until
			}
			if o.refused == 1 || o.refused%10 == 0 {
				o.log.Warn("听歌　小米拒绝读取，整个账号暂停", "音箱", w.status.Name, "连续", o.refused, "等待秒", int(wait.Seconds()), "原因", err)
			}
			return
		}
		if w.failures == 2 || w.failures%20 == 0 {
			o.log.Warn("听歌　读取失败", "音箱", w.status.Name, "连续", w.failures, "类型", xiaomi.KindOf(err), "原因", err)
		}
		return
	}
	prevOK := w.status.LastOKAt
	w.failures, w.next = 0, time.Time{}
	if o.refused > 0 {
		o.log.Info("听歌　小米恢复读取", "音箱", w.status.Name)
		o.refused, o.cool = 0, time.Time{}
	}
	w.status.LastOKAt, w.status.Error, w.status.ErrorKind = started, "", ""
	sort.Slice(recs, func(i, j int) bool { return recs[i].Time < recs[j].Time })
	if !w.baseline {
		for _, r := range recs {
			w.watermark = max(w.watermark, r.Time)
		}
		w.baseline = true
		w.heard = started // just switched on: someone may be about to talk to it
		w.status.State = StateListening
		o.log.Info("听歌　开始", "音箱", w.status.Name, "跳过旧记录", len(recs))
		return
	}
	w.status.State = StateListening
	for _, r := range recs {
		if r.Time <= w.watermark {
			continue
		}
		w.watermark = r.Time
		w.heard = started
		o.handle(ctx, id, w, r, prevOK, started)
	}
	// Nothing heard for a while: read slower until someone talks to it again. A song asked for
	// a moment ago keeps it quick, as "下一首" or "换个版本" may follow.
	idle := o.prefs.Idle && started.Sub(w.heard) > o.prefs.quietFor() && (o.Follows == nil || !o.Follows(id))
	w.status.Idle = idle
	if idle {
		w.next = started.Add(time.Duration(o.prefs.IdleMS) * time.Millisecond)
		every = time.Duration(o.prefs.IdleMS) * time.Millisecond
	}
}

// handle records one new utterance. Called with o.mu held.
func (o *Observer) handle(ctx context.Context, id string, w *watch, r xiaomi.Record, prevOK, seen time.Time) {
	in := intent.Parse(r.Query)
	if !in.Music() || in.Follow && (o.Follows == nil || !o.Follows(id)) {
		w.status.Ignored++
		if o.OnOther != nil {
			o.OnOther(id, r)
		}
		return
	}
	key := r.RequestID
	if key == "" {
		key = fmt.Sprintf("t%d|%s", r.Time, r.Query)
	}
	prev := prevOK.UnixMilli()
	if prevOK.IsZero() {
		prev = seen.UnixMilli()
	}
	lag := seen.UnixMilli() - r.Time
	c := store.Command{
		DeviceID: id, RequestKey: key, Query: r.Query, Answer: r.Answer, AnswerTypes: r.AnswerTypes,
		UpstreamMS: r.Time, PrevPollMS: prev, SeenMS: seen.UnixMilli(), Stale: lag > staleAfter.Milliseconds(),
		IntentKind: in.Kind, IntentTitle: in.Title, IntentArtist: in.Artist, IntentControl: in.Control,
		Stage: store.StageObserved,
	}
	ok, err := o.st.AddCommand(ctx, &c)
	if err != nil {
		o.log.Warn("听歌　记录没有存上", "原因", err)
		return
	}
	if ok {
		w.status.Recorded++
		o.log.Info("点歌　听到", "音箱", w.status.Name, "种类", in.Kind, "延迟ms", lag, "过时", c.Stale)
		if o.onCommand != nil {
			o.wg.Add(1)
			go func(hardware, mode string) {
				defer o.wg.Done()
				defer logx.Recover(o.log, "点歌")
				o.onCommand(ctx, c, hardware, mode)
			}(w.hardware, w.mode)
		}
	}
}

func explain(err error) string {
	if errors.Is(err, xiaomi.ErrNotLinked) {
		return "没有可用的小米登录"
	}
	switch xiaomi.KindOf(err) {
	case xiaomi.KindNetwork:
		return "连不上小米服务器"
	case xiaomi.KindAuth:
		return "小米登录已失效，需要重新登录"
	case xiaomi.KindProtocol:
		return "小米返回了看不懂的对话记录"
	default:
		return "小米拒绝了读取对话记录的请求"
	}
}

// refused is whether Xiaomi turned the read down, as against the line or the login failing.
func refused(err error) bool {
	return !errors.Is(err, xiaomi.ErrNotLinked) && xiaomi.KindOf(err) == xiaomi.KindUpstream
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
