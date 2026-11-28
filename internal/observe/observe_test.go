package observe

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"trackferry/internal/store"
	"trackferry/internal/xiaomi"
)

type fakeSource struct {
	mu     sync.Mutex
	recs   []xiaomi.Record
	err    error
	calls  int
	status xiaomi.AccountStatus
}

func (f *fakeSource) Conversations(context.Context, string, string, int) ([]xiaomi.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]xiaomi.Record(nil), f.recs...), nil
}

func (f *fakeSource) Status() xiaomi.AccountStatus { f.mu.Lock(); defer f.mu.Unlock(); return f.status }

func (f *fakeSource) set(recs []xiaomi.Record, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs, f.err = recs, err
}

func setup(t *testing.T) (*Observer, *fakeSource, *store.Store, *time.Time) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.UnixMilli(1_800_000_000_000)
	if _, err := st.SyncDevices(ctx, []store.DiscoveredDevice{{DeviceID: "d1", Name: "LX06", Alias: "书房", Hardware: "LX06"}, {DeviceID: "d2", Name: "客厅"}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetDeviceMode(ctx, "d1", store.ModeObserve); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{status: xiaomi.AccountStatus{Linked: true}}
	o := New(src, st, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	clock := now
	o.now = func() time.Time { return clock }
	return o, src, st, &clock
}

func tick(o *Observer, clock *time.Time, d time.Duration) {
	*clock = clock.Add(d)
	o.Tick(context.Background())
	o.wg.Wait()
}

func TestBaselineSkipsExistingThenRecordsNewMusicOnly(t *testing.T) {
	o, src, st, clock := setup(t)
	base := clock.UnixMilli()
	src.set([]xiaomi.Record{{RequestID: "old", Time: base - 60_000, Query: "播放晴天"}}, nil)
	tick(o, clock, 0)
	if s := o.Status(); len(s.Devices) != 1 || s.Devices[0].State != StateListening || s.Devices[0].Name != "书房" {
		t.Fatalf("status after baseline: %+v", s)
	}
	src.set([]xiaomi.Record{
		{RequestID: "new2", Time: base + 2500, Query: "今天天气怎么样"},
		{RequestID: "new1", Time: base + 1000, Query: "播放周杰伦的稻香", Answer: "好的", AnswerTypes: []string{"TTS", "AUDIO"}},
		{RequestID: "old", Time: base - 60_000, Query: "播放晴天"},
	}, nil)
	tick(o, clock, 3*time.Second)
	tick(o, clock, 3*time.Second) // same records again: nothing new
	cmds, _ := st.Commands(context.Background(), "", 0, 10)
	if len(cmds) != 1 {
		t.Fatalf("commands: %+v", cmds)
	}
	c := cmds[0]
	if c.IntentTitle != "稻香" || c.IntentArtist != "周杰伦" || c.Stage != store.StageObserved || c.Stale || c.UpstreamMS != base+1000 || c.PrevPollMS != base || c.SeenMS != base+3000 {
		t.Fatalf("command: %+v", c)
	}
	s := o.Status().Devices[0]
	if s.Recorded != 1 || s.Ignored != 1 {
		t.Fatalf("counts: %+v", s)
	}
}

func TestFailedReadDoesNotSetBaseline(t *testing.T) {
	o, src, st, clock := setup(t)
	base := clock.UnixMilli()
	src.set(nil, &xiaomi.Error{Kind: xiaomi.KindNetwork, Detail: "down"})
	tick(o, clock, 0)
	tick(o, clock, 5*time.Second)
	if s := o.Status().Devices[0]; s.State != StateError || s.Error == "" {
		t.Fatalf("status: %+v", s)
	}
	// Reads recover: history visible now must not be replayed.
	src.set([]xiaomi.Record{{RequestID: "a", Time: base - 1000, Query: "播放晴天"}}, nil)
	tick(o, clock, time.Minute)
	if cmds, _ := st.Commands(context.Background(), "", 0, 10); len(cmds) != 0 {
		t.Fatalf("history replayed: %+v", cmds)
	}
}

func TestPausedWithoutLoginAndReenableResetsWatermark(t *testing.T) {
	o, src, st, clock := setup(t)
	src.mu.Lock()
	src.status = xiaomi.AccountStatus{Linked: true, RelinkRequired: true}
	src.mu.Unlock()
	tick(o, clock, 0)
	if s := o.Status(); s.Paused != xiaomi.SyncRelinkRequired || s.Devices[0].State != StatePaused || src.calls != 0 {
		t.Fatalf("status: %+v calls %d", s, src.calls)
	}
	// Switching off removes the watch.
	st.SetDeviceMode(context.Background(), "d1", store.ModeOff)
	tick(o, clock, time.Second)
	if s := o.Status(); len(s.Devices) != 0 {
		t.Fatalf("still watching: %+v", s)
	}
}

func TestStaleRecordFlagged(t *testing.T) {
	o, src, st, clock := setup(t)
	base := clock.UnixMilli()
	src.set([]xiaomi.Record{}, nil)
	tick(o, clock, 0)
	src.set([]xiaomi.Record{{Time: base + 1, Query: "播放晴天"}}, nil)
	tick(o, clock, 10*time.Minute)
	cmds, _ := st.Commands(context.Background(), "", 0, 10)
	if len(cmds) != 1 || !cmds[0].Stale {
		t.Fatalf("commands: %+v", cmds)
	}
}

func TestSleepWindowAndSwitchOffStopReads(t *testing.T) {
	o, src, _, clock := setup(t)
	p := DefaultPrefs()
	p.Sleep, p.SleepFrom, p.SleepTo = true, "23:00", "07:00"
	o.Apply(p)
	*clock = time.Date(2026, 11, 28, 23, 30, 0, 0, time.Local)
	tick(o, clock, 0)
	if s := o.Status(); s.Paused != "sleep" || src.calls != 0 {
		t.Fatalf("read while asleep: %+v calls %d", s, src.calls)
	}
	*clock = time.Date(2026, 11, 29, 7, 0, 0, 0, time.Local)
	tick(o, clock, 0)
	if s := o.Status(); s.Paused != "" || src.calls != 1 {
		t.Fatalf("not awake at 07:00: %+v calls %d", s, src.calls)
	}
	p.Listen = false
	o.Apply(p)
	tick(o, clock, time.Minute)
	if s := o.Status(); s.Paused != "off" || src.calls != 1 {
		t.Fatalf("read while switched off: %+v calls %d", s, src.calls)
	}
}

func TestPrefsCheck(t *testing.T) {
	for _, c := range []struct {
		edit func(*Prefs)
		ok   bool
	}{
		{func(*Prefs) {}, true},
		{func(p *Prefs) { p.IntervalMS = 500 }, false},
		{func(p *Prefs) { p.IdleMS = 1500 }, false},
		{func(p *Prefs) { p.QuietMin = 0 }, false},
		{func(p *Prefs) { p.QuietMin = 61 }, false},
		{func(p *Prefs) { p.QuietMin = 3 }, true},
		{func(p *Prefs) { p.SleepFrom = "25:00" }, false},
		{func(p *Prefs) { p.SleepTo = p.SleepFrom }, false},
	} {
		p := DefaultPrefs()
		c.edit(&p)
		if (p.Check() == "") != c.ok {
			t.Errorf("%+v: %q", p, p.Check())
		}
	}
}

func TestQuietSpeakerIsReadSlowerUntilItHearsSomething(t *testing.T) {
	o, src, _, clock := setup(t)
	o.Apply(DefaultPrefs())
	base := clock.UnixMilli()
	tick(o, clock, 0) // baseline: hot
	tick(o, clock, HotFor+time.Second)
	if s := o.Status(); !s.Devices[0].Idle {
		t.Fatalf("not idle after %v: %+v", HotFor, s)
	}
	calls := src.calls
	tick(o, clock, 3*time.Second) // inside the idle interval
	if src.calls != calls {
		t.Fatal("read again before the idle interval")
	}
	src.set([]xiaomi.Record{{RequestID: "w", Time: base + int64(HotFor/time.Millisecond) + 5000, Query: "今天天气怎么样"}}, nil)
	tick(o, clock, DefaultIdleMS*time.Millisecond)
	if s := o.Status(); s.Devices[0].Idle {
		t.Fatalf("still idle after hearing something: %+v", s)
	}
}

func TestQuietMinutesAreTheListeners(t *testing.T) {
	o, _, _, clock := setup(t)
	p := DefaultPrefs()
	p.QuietMin = 3
	o.Apply(p)
	tick(o, clock, 0)
	tick(o, clock, 2*time.Minute)
	if o.Status().Devices[0].Idle {
		t.Fatal("idle before 3 minutes")
	}
	tick(o, clock, time.Minute+time.Second)
	if !o.Status().Devices[0].Idle {
		t.Fatal("not idle after 3 minutes")
	}
}

func TestRefusalPausesTheWholeAccount(t *testing.T) {
	o, src, st, clock := setup(t)
	if _, err := st.SetDeviceMode(context.Background(), "d2", store.ModeObserve); err != nil {
		t.Fatal(err)
	}
	tick(o, clock, 0)
	src.set(nil, &xiaomi.Error{Kind: xiaomi.KindUpstream, Detail: "busy"})
	tick(o, clock, time.Second)
	calls := src.calls
	tick(o, clock, time.Second) // within the account's wait: neither speaker is read
	if src.calls != calls {
		t.Fatalf("read during the account's wait: %d → %d", calls, src.calls)
	}
	src.set(nil, nil)
	tick(o, clock, maxRefused)
	if o.refused != 0 || !o.cool.IsZero() {
		t.Fatalf("wait not lifted after a good read: %d %v", o.refused, o.cool)
	}
}
