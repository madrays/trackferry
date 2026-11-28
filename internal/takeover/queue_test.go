package takeover

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"trackferry/internal/intent"
	"trackferry/internal/online"
	"trackferry/internal/routing"
	"trackferry/internal/store"
	"trackferry/internal/stream"
)

type queueStarter struct {
	mu      sync.Mutex
	started []Request
	idle    bool
}

func (s *queueStarter) Start(_ context.Context, r Request) (store.TakeoverRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, r)
	return store.TakeoverRun{}, nil
}
func (s *queueStarter) Release(string)                                         {}
func (s *queueStarter) Quiet(string) func(bool)                                { return func(bool) {} }
func (s *queueStarter) Hear(string, Heard)                                     {}
func (s *queueStarter) Playing(string) bool                                    { return !s.idle }
func (s *queueStarter) Say(_ context.Context, _ string, w func() string) error { w(); return nil }

func (s *queueStarter) last() Request { return s.started[len(s.started)-1] }

func queueSetup(t *testing.T) (*Dispatcher, *queueStarter, *queue, *[]int64) {
	_, _, st := setup(t)
	ctx := context.Background()
	if _, err := st.SyncDevices(ctx, []store.DiscoveredDevice{{DeviceID: "d1", Name: "音箱", Hardware: "LX06"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetDeviceMode(ctx, "d1", store.ModeTakeover); err != nil {
		t.Fatal(err)
	}
	sp := &queueStarter{}
	streams := stream.New("http://nas", t.TempDir())
	var asked []int64
	d := &Dispatcher{Engine: sp, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Streams: streams, Allowed: func(string) bool { return true },
		Open: func(*routing.Route) stream.Opener { return nil }, Next: func(id int64, list string) (*routing.Route, bool) {
			asked = append(asked, id)
			return routing.ForTest("歌", "人"), id != 3 // 3 cannot be read
		}}
	q := &queue{dev: "d1", hardware: "LX06", list: "歌单“测试”", ids: []int64{1, 2, 3, 4}}
	d.enqueue(q)
	return d, sp, q, &asked
}

func TestWebListStartsAtSelectedTrackInBothModes(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{ModeLoop, ModeShuffle, ModeOne} {
		t.Run(mode, func(t *testing.T) {
			d, _, _, asked := queueSetup(t)
			dev, err := d.Store.Device(ctx, "d1")
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Store.SetSetting(ctx, settingMode+dev.DeviceID, mode); err != nil {
				t.Fatal(err)
			}
			order := []int64{1, 2, 4, 5}
			if _, err := d.PlayList(ctx, dev, "歌单", order, 4); err != nil {
				t.Fatal(err)
			}
			q := d.queued(dev.DeviceID)
			if !slices.Equal(*asked, []int64{4}) || q.ids[q.pos] != 4 || !slices.Equal(q.order, order) || len(q.ids) != len(order) {
				t.Fatal(*asked, q.ids, q.pos, q.order)
			}
			if mode != ModeShuffle && !slices.Equal(q.ids, order) {
				t.Fatal("changed the original order", q.ids)
			}
			if _, err := d.PlayList(ctx, dev, "歌单", order, 99); !errors.Is(err, ErrMoved) || d.queued(dev.DeviceID) != q {
				t.Fatal("a stale selection changed playback", err)
			}
		})
	}
}

func TestWebQueueMovesTracksWithNextAndLast(t *testing.T) {
	d, _, q, asked := queueSetup(t)
	ctx := context.Background()
	q.order = slices.Clone(q.ids)
	q.pos = 1
	if !d.QueueTrack("d1", 4, true) || !d.QueueTrack("d1", 1, false) || !d.QueueTrack("d1", 2, false) {
		t.Fatal("the active queue did not accept the edit")
	}
	if !slices.Equal(q.ids, []int64{2, 4, 3, 1}) || q.pos != 0 {
		t.Fatal(q.ids, q.pos)
	}
	if err := d.Skip(ctx, "d1", 1); err != nil || !slices.Equal(*asked, []int64{4}) {
		t.Fatal(err, *asked)
	}
}

func TestListPlaysOnAndStartsOver(t *testing.T) {
	d, sp, q, asked := queueSetup(t)
	d.turn(q)(EndPlayed) // 1 is through: 2
	if r := sp.last(); !r.Follows || r.Song.List != "歌单“测试”" || r.Kind != "push" || (*asked)[0] != 2 {
		t.Fatalf("%+v %v", r, *asked)
	}
	sp.last().Ended(EndPlayed) // 3 is skipped: 4
	if n := len(*asked); n != 3 || (*asked)[2] != 4 {
		t.Fatal(*asked)
	}
	for range 40 { // round and round, never the same twice in a row
		was := q.ids[q.pos]
		sp.last().Ended(EndPlayed)
		if q.ids[q.pos] == was || q.ids[q.pos] == 3 {
			t.Fatal(was, q.ids, q.pos)
		}
	}
	if len(sp.started) != 42 {
		t.Fatal(len(sp.started))
	}
	// A song that was replaced says nothing about the one that is on.
	sp.started[0].Ended(EndOther)
	if d.queued("d1") != q {
		t.Fatal("dropped by an old song")
	}
	sp.last().Ended(EndOther)
	if d.queued("d1") != nil || len(sp.started) != 42 {
		t.Fatal("went on after it was stopped")
	}
}

func TestListGivesUpOnSilence(t *testing.T) {
	d, sp, q, _ := queueSetup(t)
	end := d.turn(q)
	for range 10 {
		end(EndUnplayed)
		if len(sp.started) == 0 || d.queued("d1") == nil {
			break
		}
		end = sp.last().Ended
	}
	if len(sp.started) != maxFails-1 || d.queued("d1") != nil {
		t.Fatal(len(sp.started))
	}
}

func TestListEndsWhenTakeoverDoes(t *testing.T) {
	d, sp, q, _ := queueSetup(t)
	d.Store.SetDeviceMode(context.Background(), "d1", store.ModeObserve)
	d.turn(q)(EndPlayed)
	if len(sp.started) != 0 || d.queued("d1") != nil {
		t.Fatal("played on")
	}
}

func TestNextPreviousAndANewRequestWhileAListPlays(t *testing.T) {
	d, sp, q, asked := queueSetup(t)
	ctx := context.Background()
	old := d.turn(q)
	say := func(key, kind, control string) {
		c := store.Command{DeviceID: "d1", RequestKey: key, Query: key, UpstreamMS: 1, SeenMS: 2, IntentKind: kind, IntentControl: control, Stage: store.StageObserved}
		if _, err := d.Store.AddCommand(ctx, &c); err != nil {
			t.Fatal(err)
		}
		d.OnCommand(ctx, c, "LX06", store.ModeTakeover)
	}
	say("下一首", intent.KindControl, "next")
	if len(sp.started) != 1 || sp.last().Follows || sp.last().Kind != "command" || (*asked)[0] != 2 {
		t.Fatal(sp.started, *asked)
	}
	old(EndOther) // the song that was cut off
	say("上一首", intent.KindControl, "previous")
	if len(sp.started) != 2 || (*asked)[1] != 1 || d.queued("d1") != q {
		t.Fatal(*asked)
	}
	say("暂停", intent.KindControl, "pause")
	say("收藏这首歌", intent.KindFavorite, "add")
	if d.queued("d1") != q {
		t.Fatal("a pause ended the list")
	}
	d.Router = &fakeDecider{found: true}
	say("播放晴天", intent.KindSong, "")
	if now := d.queued("d1"); now == q || now == nil || !now.solo {
		t.Fatal("still there")
	}
	d.Random = func(int) []int64 { return []int64{8} }
	n := len(sp.started)
	say("下一首了", intent.KindControl, "next") // a single song of ours: another one
	if len(sp.started) != n+1 || sp.last().Kind != "command" {
		t.Fatal("did not move on from the single song")
	}
	sp.last().Ended(EndOther)
	n = len(sp.started)
	say("再下一首", intent.KindControl, "next") // Xiao AI's
	if len(sp.started) != n {
		t.Fatal("no list, yet it played")
	}
}

func TestPutTheSongIntoAPlaylist(t *testing.T) {
	d, _, _, _ := queueSetup(t)
	ctx := context.Background()
	if _, s := d.put(ctx, "d1", "测试", true); s != "没听出是哪首歌" {
		t.Fatal(s)
	}
	d.Remember("d1", "晴天", "周杰伦", 0)
	if s, _ := d.put(ctx, "d1", "测试", true); !strings.Contains(s, "不在曲库") {
		t.Fatal(s)
	}
	d.Remember("d1", "晴天", "周杰伦", 7)
	in := false
	d.Put = func(_ context.Context, id int64, name string, on bool) (string, bool, error) {
		if name != "测试" {
			return "", false, errors.New("飞牛音乐里没有叫“" + name + "”的歌单，有测试")
		}
		changed := in != on
		in = on
		return "测试", changed, nil
	}
	for _, x := range []struct {
		name, said string
		on         bool
	}{{"测试", "已加入测试歌单", true}, {"测试", "已经在测试歌单里了", true}, {"测试", "已移出测试歌单", false},
		{"开车", "飞牛音乐里没有叫“开车”的歌单，有测试", true}} {
		if _, s := d.put(ctx, "d1", x.name, x.on); s != x.said {
			t.Error(s)
		}
	}
}

// The engine's side: a song that plays through calls for the next; one that is stopped does not.
func TestRunTellsHowItEnded(t *testing.T) {
	for _, c := range []struct {
		name string
		do   func(*Engine, *fakeSpeaker)
		want End
	}{
		{"played", func(*Engine, *fakeSpeaker) {}, EndPlayed},
		{"stopped", func(e *Engine, _ *fakeSpeaker) { e.Hear("d1", Heard{Control: "stop"}) }, EndOther},
		{"something else", func(_ *Engine, sp *fakeSpeaker) { sp.mu.Lock(); sp.playing = "native"; sp.mu.Unlock() }, EndOther},
		{"not fetched", nil, EndUnplayed},
	} {
		e, sp, _ := setup(t)
		r := req(sp)
		r.Follows = true
		r.Track.Duration = 500 * time.Millisecond
		sp.playing = ""
		got := make(chan End, 1)
		r.Ended = func(h End) { got <- h }
		if sp.fetchOK = c.do != nil; c.do != nil {
			time.AfterFunc(400*time.Millisecond, func() { c.do(e, sp) })
		}
		e.Start(context.Background(), r)
		select {
		case h := <-got:
			if h != c.want {
				t.Errorf("%s: %v", c.name, h)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: never ended", c.name)
		}
		sp.mu.Lock()
		if len(sp.volumes) != 0 || sp.calls[0] != "play" {
			t.Errorf("%s: %v", c.name, sp.calls)
		}
		sp.mu.Unlock()
	}
}

func TestOnlineSingerPlaysOnRoundAndRound(t *testing.T) {
	d, _, _, _ := queueSetup(t)
	var took []string
	d.Online = func(_ context.Context, key string) (*routing.Route, error) {
		took = append(took, key)
		return routing.ForTest(key, "陈奕迅"), nil
	}
	first := routing.ForTest("好久不见", "陈奕迅")
	first.List = "陈奕迅的歌"
	q := onlineQueue("d1", "LX06", first, []online.Candidate{{Key: "kuwo:3", Title: "富士山下", Artist: "陈奕迅"}})
	if len(q.ids) != 2 || q.list != "陈奕迅的歌" || q.outside[q.ids[1]].Online != "kuwo:3" {
		t.Fatalf("%+v", q)
	}
	d.follow(q)
	for range 2 {
		if _, ok := d.step(q, 1); !ok {
			t.Fatal("no next song")
		}
	}
	// The first song comes back too, found anew.
	if len(took) != 2 || took[0] != "kuwo:3" || took[1] != q.outside[q.ids[0]].Online {
		t.Fatalf("took %v", took)
	}
}
