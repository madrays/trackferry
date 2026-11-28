package takeover

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"trackferry/internal/intent"
	"trackferry/internal/routing"
	"trackferry/internal/store"
	"trackferry/internal/stream"
	"trackferry/internal/xiaomi"
)

// fakeSpeaker models a speaker whose player Xiao AI may grab back.
type fakeSpeaker struct {
	mu       sync.Mutex
	volume   int
	least    int    // what 0 leads to
	playing  string // audio id currently playing, "" = idle
	calls    []string
	volumes  []int
	fetch    chan struct{} // closed on the first PlayURL (the speaker pulls the stream)
	late     bool
	fetchOK  bool // whether PlayURL leads to a fetch
	hijacks  int  // how many times Xiao AI's playlist takes over right after a push
	playErr  error
	speakErr error
	statusOK bool
	playHook func()
	volHook  func(v int) error // an answer that got lost: the volume is set all the same
}

func newSpeaker() *fakeSpeaker {
	return &fakeSpeaker{volume: 40, playing: "native-qq", fetch: make(chan struct{}), fetchOK: true, statusOK: true}
}

func (f *fakeSpeaker) log(c string) { f.calls = append(f.calls, c) }

func (f *fakeSpeaker) PlayStatus(context.Context, string) (xiaomi.PlayStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("status")
	if f.late { // the answer overtaken by the mute
		f.mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		f.mu.Lock()
	}
	if !f.statusOK {
		return xiaomi.PlayStatus{}, errors.New("status unavailable")
	}
	p := xiaomi.PlayStatus{Volume: f.volume, AudioID: f.playing, Position: 1000}
	if f.playing != "" {
		p.Status = xiaomi.PlayPlaying
	}
	return p, nil
}

func (f *fakeSpeaker) PlayerOp(_ context.Context, _ string, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(action)
	if action == "stop" {
		f.playing = ""
	}
	return nil
}

func (f *fakeSpeaker) SetVolume(_ context.Context, _ string, v int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("volume")
	f.volume = max(v, f.least)
	f.volumes = append(f.volumes, v)
	if f.volHook != nil {
		return f.volHook(v)
	}
	return nil
}

func (f *fakeSpeaker) Speak(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(fmt.Sprintf("speak@%d:%s", f.volume, text))
	return f.speakErr
}

func (f *fakeSpeaker) PlayURL(context.Context, string, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("play")
	if f.playHook != nil {
		f.playHook()
	}
	if f.playErr != nil {
		return f.playErr
	}
	f.playing = xiaomi.OurAudioID
	if f.hijacks > 0 {
		f.hijacks--
		f.playing = "native-qq" // Xiao AI's delayed playlist lands after our push
	}
	if f.fetchOK {
		select {
		case <-f.fetch:
		default:
			close(f.fetch)
		}
	}
	return nil
}

var fast = Timing{Settle: 5 * time.Millisecond, Guard: 300 * time.Millisecond, Poll: 10 * time.Millisecond, UnmuteAfter: 100 * time.Millisecond, MaxReplays: 2, EndSlack: 10 * time.Millisecond, MaxWatch: 2 * time.Second}

func setup(t *testing.T) (*Engine, *fakeSpeaker, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sp := newSpeaker()
	return New(sp, st, slog.New(slog.NewTextHandler(io.Discard, nil)), fast), sp, st
}

func req(sp *fakeSpeaker) Request {
	return Request{DeviceID: "d1", Hardware: "LX06", Kind: "test", Track: Track{URL: "http://nas/stream/x.wav", Label: "提示音", Fetched: sp.fetch}}
}

func steps(r store.TakeoverRun) string {
	var s []string
	for _, st := range r.Steps {
		s = append(s, st.Step)
	}
	return strings.Join(s, ",")
}

func TestUnconfirmedHandoffStopsItsOwnStream(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "missing fetch"
		if cancel {
			name = "request cancelled after push"
		}
		t.Run(name, func(t *testing.T) {
			e, sp, _ := setup(t)
			e.timing.MaxReplays = 0
			sp.fetchOK = cancel
			done := make(chan End, 1)
			r := req(sp)
			r.Handoff, r.Ended = true, func(end End) { done <- end }
			ctx := context.Background()
			run, err := e.Start(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			if cancel {
				select {
				case <-sp.fetch:
					e.CancelRun(r.DeviceID, run.ID)
				case <-time.After(time.Second):
					t.Fatal("speaker did not fetch")
				}
			}
			select {
			case end := <-done:
				if end != EndOther {
					t.Fatal("failed handoff advanced the queue", end)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("handoff did not finish")
			}
			if p, _ := sp.PlayStatus(ctx, r.DeviceID); p.Playing() {
				t.Fatal("failed destination still playing", p)
			}
		})
	}
}

func TestHandoffDoesNotPauseReplacedOrNativePlayback(t *testing.T) {
	e, sp, _ := setup(t)
	d := e.device("d1")
	d.runID, d.watching, d.cancel = 2, true, func(error) {}
	for _, id := range []int64{1, 2} {
		if err := e.PauseRun(context.Background(), "d1", "LX06", id); !errors.Is(err, ErrSeekMoved) {
			t.Fatalf("run %d: %v", id, err)
		}
	}
	if p, _ := sp.PlayStatus(context.Background(), "d1"); p.AudioID != "native-qq" {
		t.Fatal("native playback was interrupted", p)
	}
}

func TestTakeoverMutesStopsPlaysConfirmsAndRestores(t *testing.T) {
	e, sp, st := setup(t)
	run, err := e.Run(context.Background(), req(sp))
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != store.OutcomeConfirmed {
		t.Fatalf("outcome %s %s: %s", run.Outcome, run.Detail, steps(run))
	}
	if got := steps(run); got != "status,mute,stop1,stop2,play,fetched,confirmed,restore" {
		t.Fatalf("steps %s", got)
	}
	if len(sp.volumes) != 2 || sp.volumes[0] != 0 || sp.volumes[1] != 40 {
		t.Fatalf("volumes %v", sp.volumes)
	}
	// Muting and stopping go out together, all before the song.
	before, _, _ := strings.Cut(strings.Join(sp.calls, ","), "play")
	if strings.Count(before, "volume") != 1 || strings.Count(before, "pause") != 2 || strings.Count(before, "stop") != 2 {
		t.Fatalf("calls %v", sp.calls)
	}
	if _, ok, _ := st.PendingVolume(context.Background(), "d1"); ok {
		t.Fatal("pending volume left behind")
	}
	saved, _ := st.Run(context.Background(), run.ID)
	if saved.Outcome != store.OutcomeConfirmed || len(saved.Steps) != len(run.Steps) {
		t.Fatalf("saved %+v", saved)
	}
}

func TestTakeoverFightsXiaoAIsPlaylist(t *testing.T) {
	e, sp, _ := setup(t)
	sp.hijacks = 1
	run, _ := e.Run(context.Background(), req(sp))
	if run.Outcome != store.OutcomeConfirmed || !strings.Contains(steps(run), "foreign,replay") {
		t.Fatalf("%s %s", run.Outcome, steps(run))
	}
	if sp.volume != 40 {
		t.Fatalf("volume %d", sp.volume)
	}
}

func TestTakeoverGivesUpWhenXiaoAIKeepsWinning(t *testing.T) {
	e, sp, _ := setup(t)
	sp.hijacks = 10
	run, _ := e.Run(context.Background(), req(sp))
	if run.Outcome != store.OutcomeFailed || sp.volume != 40 {
		t.Fatalf("%s volume %d: %s", run.Outcome, sp.volume, steps(run))
	}
}

func TestTakeoverPlayRefusedStillRestoresVolume(t *testing.T) {
	e, sp, _ := setup(t)
	sp.playErr = &xiaomi.DeviceError{Code: 3}
	run, _ := e.Run(context.Background(), req(sp))
	if run.Outcome != store.OutcomeFailed || !strings.Contains(run.Detail, "音箱拒绝") || sp.volume != 40 {
		t.Fatalf("%s %q volume %d", run.Outcome, run.Detail, sp.volume)
	}
}

func TestTakeoverWithoutFetchIsOnlyDispatched(t *testing.T) {
	e, sp, _ := setup(t)
	sp.fetchOK = false
	run, _ := e.Run(context.Background(), req(sp))
	if run.Outcome != store.OutcomeDispatched || sp.volume != 40 {
		t.Fatalf("%s volume %d", run.Outcome, sp.volume)
	}
}

func TestTakeoverWithoutStatusDoesNotMute(t *testing.T) {
	e, sp, _ := setup(t)
	sp.statusOK = false
	run, _ := e.Run(context.Background(), req(sp))
	if len(sp.volumes) != 0 {
		t.Fatalf("muted without knowing the volume: %v", sp.volumes)
	}
	if run.Outcome != store.OutcomeFetched {
		t.Fatalf("%s %s", run.Outcome, steps(run))
	}
}

func TestNewerRunCancelsOlderAndVolumeSurvives(t *testing.T) {
	e, sp, st := setup(t)
	ctx := context.Background()
	release := make(chan struct{})
	sp.playHook = func() {
		sp.playHook = nil
		sp.mu.Unlock()
		<-release // first push hangs until the second run arrives
		sp.mu.Lock()
	}
	first, err := e.Start(ctx, req(sp))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	sp2 := req(sp)
	sp2.Track.Fetched = make(chan struct{}) // a fresh stream for the second run
	second, _ := e.Start(ctx, sp2)
	close(release)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := st.Run(ctx, first.ID)
		b, _ := st.Run(ctx, second.ID)
		if a.Outcome != store.OutcomeRunning && b.Outcome != store.OutcomeRunning {
			if a.Outcome != store.OutcomeCancelled {
				t.Fatalf("first %s: %s", a.Outcome, steps(a))
			}
			if b.Outcome == store.OutcomeFailed || b.Outcome == store.OutcomeCancelled {
				t.Fatalf("second %s: %s", b.Outcome, steps(b))
			}
			sp.mu.Lock()
			defer sp.mu.Unlock()
			if sp.volume != 40 {
				t.Fatalf("volume %d after runs %v", sp.volume, sp.volumes)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runs did not finish")
}

func TestStopsTheRepeatOnceTheTrackEnds(t *testing.T) {
	e, sp, _ := setup(t)
	r := req(sp)
	r.Track.Duration = 50 * time.Millisecond
	run, _ := e.Run(context.Background(), r)
	if run.Outcome != store.OutcomeConfirmed || !strings.HasSuffix(steps(run), "restore,ended") {
		t.Fatalf("%s: %s", run.Outcome, steps(run))
	}
	if sp.playing != "" || !run.Steps[len(run.Steps)-1].OK {
		t.Fatalf("still playing %q", sp.playing)
	}
}

func TestStopsWhenTheSpeakerStartsOver(t *testing.T) {
	e, sp, _ := setup(t)
	r := req(sp)
	again := make(chan struct{})
	r.Track.Restarted = again
	time.AfterFunc(400*time.Millisecond, func() { close(again) })
	run, _ := e.Run(context.Background(), r)
	last := run.Steps[len(run.Steps)-1]
	if last.Step != StepEnded || !strings.Contains(last.Detail, "从头") || sp.playing != "" {
		t.Fatalf("%s %q playing %q", steps(run), last.Detail, sp.playing)
	}
}

func TestLeavesOtherPlaybackAloneAtTheEnd(t *testing.T) {
	e, sp, _ := setup(t)
	r := req(sp)
	r.Track.Duration = 700 * time.Millisecond
	sp.playHook = func() {
		go func() { // after the guard, the user asks Xiao AI for something else while ours plays
			time.Sleep(450 * time.Millisecond)
			sp.mu.Lock()
			sp.playing = "native-qq"
			sp.mu.Unlock()
		}()
	}
	run, _ := e.Run(context.Background(), r)
	if last := run.Steps[len(run.Steps)-1]; sp.playing != "native-qq" || !strings.Contains(last.Detail, "别的") {
		t.Fatalf("stopped other playback: %s %q", steps(run), last.Detail)
	}
}

func TestRecoverVolumesAfterCrash(t *testing.T) {
	e, sp, st := setup(t)
	ctx := context.Background()
	sp.volume = 0
	st.SetPendingVolume(ctx, "d1", 55, time.Now())
	e.recoverOnce(ctx)
	if sp.volume != 55 {
		t.Fatalf("volume %d", sp.volume)
	}
	if all, _ := st.PendingVolumes(ctx); len(all) != 0 {
		t.Fatal(all)
	}
}

func TestDispatcherOnlyActsOnEnabledTakeoverSpeakers(t *testing.T) {
	e, _, st := setup(t)
	ctx := context.Background()
	add := func(key string, stale bool, kind string) store.Command {
		c := store.Command{DeviceID: "d1", RequestKey: key, Query: "播放晴天", UpstreamMS: 1, PrevPollMS: 1, SeenMS: 2, Stale: stale, IntentKind: kind, Stage: store.StageObserved}
		if _, err := st.AddCommand(ctx, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	stageOf := func(id int64) string {
		cmds, _ := st.Commands(ctx, "", 0, 50)
		for _, c := range cmds {
			if c.ID == id {
				return c.Stage
			}
		}
		return "?"
	}
	streams := stream.New("http://nas", t.TempDir())
	fd := &fakeDecider{found: true}
	fs := &fakeStarter{}
	allowed := true
	d := &Dispatcher{Engine: fs, Store: st, Log: e.log, Router: fd, Streams: streams, Allowed: func(string) bool { return allowed },
		Open: func(*routing.Route) stream.Opener { return nil }}
	routeOf := func(id int64) string {
		cmds, _ := st.Commands(ctx, "", 0, 50)
		for _, c := range cmds {
			if c.ID == id {
				return string(c.Route)
			}
		}
		return ""
	}

	c := add("a", false, "song")
	d.OnCommand(ctx, c, "LX06", store.ModeObserve)
	if stageOf(c.ID) != store.StageObserved || len(fs.started) != 0 || !strings.Contains(routeOf(c.ID), `"preview":true`) {
		t.Fatalf("observe mode: %s %s", stageOf(c.ID), routeOf(c.ID))
	}
	c = add("b", true, "song")
	d.OnCommand(ctx, c, "LX06", store.ModeTakeover)
	if stageOf(c.ID) != store.StageSkipped || len(fs.started) != 0 {
		t.Fatal("stale command acted on")
	}
	c = add("c", false, "song")
	d.OnCommand(ctx, c, "LX06", store.ModeTakeover)
	if stageOf(c.ID) != store.StageTakenOver || len(fs.started) != 1 || fs.started[0].Track.Duration != 0 {
		t.Fatalf("stage %s %+v", stageOf(c.ID), fs.started)
	}
	allowed = false
	c = add("d", false, "song")
	d.OnCommand(ctx, c, "LX06", store.ModeTakeover)
	if len(fs.started) != 1 {
		t.Fatal("a speaker that is not released was taken over")
	}
	allowed = true
	fd.found = false
	c = add("e", false, "song")
	d.OnCommand(ctx, c, "LX06", store.ModeTakeover)
	if stageOf(c.ID) != store.StageNative || fs.released != 1 {
		t.Fatal("nothing to play should leave it to Xiao AI and stop guarding")
	}
	c = add("f", false, "control")
	d.OnCommand(ctx, c, "LX06", store.ModeTakeover)
	if stageOf(c.ID) != store.StageSkipped || fs.released != 2 {
		t.Fatal("control should release the guard")
	}

	// A newer command cancels the routing of an older one.
	fd.found, fd.block = true, make(chan struct{})
	old := add("g", false, "song")
	finished := make(chan struct{})
	go func() { d.OnCommand(ctx, old, "LX06", store.ModeTakeover); close(finished) }()
	for fd.calls.Load() < 5 {
		time.Sleep(time.Millisecond)
	}
	c = add("h", false, "control")
	d.OnCommand(ctx, c, "LX06", store.ModeTakeover)
	<-finished
	if len(fs.started) != 1 || stageOf(old.ID) != store.StageSkipped {
		t.Fatalf("old command still acted: %d %s", len(fs.started), stageOf(old.ID))
	}
}

func TestReleaseStopsGuarding(t *testing.T) {
	e, sp, st := setup(t)
	ctx := context.Background()
	run, err := e.Start(ctx, Request{DeviceID: "d1", Hardware: "LX06", Kind: "command", Track: Track{URL: "http://nas/stream/x.mp3", Fetched: sp.fetch}})
	if err != nil {
		t.Fatal(err)
	}
	for range 500 {
		if r, _ := st.Run(ctx, run.ID); len(r.Steps) > 0 && r.Steps[len(r.Steps)-1].Step == StepRestore {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	e.Release("d1")
	e.Release("d1") // harmless twice
	for range 500 {
		if r, _ := st.Run(ctx, run.ID); r.Outcome != store.OutcomeRunning {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("run did not finish")
}

type fakeDecider struct {
	found bool
	block chan struct{}
	calls atomic.Int32
}

func (f *fakeDecider) Decide(ctx context.Context, _ intent.Intent, _ string) *routing.Route {
	f.calls.Add(1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	if !f.found {
		return &routing.Route{Decision: routing.Native, Layers: []routing.Layer{}}
	}
	return routing.ForTest("晴天", "周杰伦")
}

type fakeStarter struct {
	started  []Request
	released int
}

func (f *fakeStarter) Start(_ context.Context, r Request) (store.TakeoverRun, error) {
	f.started = append(f.started, r)
	return store.TakeoverRun{}, nil
}
func (f *fakeStarter) Release(string)          { f.released++ }
func (f *fakeStarter) Hear(string, Heard)      {}
func (f *fakeStarter) Quiet(string) func(bool) { return func(bool) {} }

func (f *fakeStarter) Playing(string) bool { return false }

func (f *fakeStarter) Say(_ context.Context, _ string, work func() string) error {
	work()
	return nil
}

// A conversation over the song: the speaker hangs up, answers, and is left silent.
func TestContinuesAfterSomeoneTalkedToXiaoAI(t *testing.T) {
	e, sp, st := setup(t)
	e.timing.Look, e.timing.LookClose, e.timing.Speak, e.timing.Pause = 10*time.Millisecond, 10*time.Millisecond, time.Millisecond, 30*time.Millisecond
	r := req(sp)
	r.Track.Length = time.Hour
	got := make(chan time.Duration, 1)
	r.Track.Resume = func(_ context.Context, at time.Duration) (Track, time.Duration, error) {
		got <- at
		c := make(chan struct{})
		close(c)
		return Track{URL: "http://nas/stream/rest.mp3", Fetched: c}, at, nil
	}
	run, err := e.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond) // past the guard
	e.Hear("d1", Heard{Answer: 10})
	sp.mu.Lock()
	sp.playing = ""
	sp.mu.Unlock()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the song was not continued")
	}
	time.Sleep(100 * time.Millisecond)
	saved, _ := st.Run(context.Background(), run.ID)
	if s := steps(saved); !strings.Contains(s, "talked,paused,resume") && !strings.Contains(s, "paused,talked,resume") {
		t.Fatalf("steps %s", s)
	}
	// Asked for quiet: it stays quiet.
	e.Hear("d1", Heard{Stop: true})
	sp.mu.Lock()
	sp.playing = ""
	sp.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	select {
	case <-got:
		t.Fatal("continued a song the listener stopped")
	default:
	}
	// Nothing was said: a pause on the speaker itself is the listener's own.
}

// Soon after the start, Xiao AI may take the song for words and stop it with an answer that
// leaves no record (#20): the song goes on once she is done, one time only.
func TestContinuesAfterXiaoAICutsInUnheard(t *testing.T) {
	e, sp, st := setup(t)
	e.timing.Look, e.timing.LookClose, e.timing.Speak, e.timing.Pause = 10*time.Millisecond, 10*time.Millisecond, time.Millisecond, 30*time.Millisecond
	r := req(sp)
	r.Track.Length = time.Hour
	got := make(chan time.Duration, 4)
	r.Track.Resume = func(_ context.Context, at time.Duration) (Track, time.Duration, error) {
		got <- at
		c := make(chan struct{})
		close(c)
		return Track{URL: "http://nas/stream/rest.mp3", Fetched: c}, at, nil
	}
	run, err := e.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond) // past the guard, well inside the window
	sp.mu.Lock()
	sp.playing = ""
	sp.mu.Unlock()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the song was not continued")
	}
	time.Sleep(100 * time.Millisecond)
	saved, _ := st.Run(context.Background(), run.ID)
	if s := steps(saved); !strings.Contains(s, "paused,talked,resume") {
		t.Fatalf("steps %s", s)
	}
	// A second stop is the listener's own.
	sp.mu.Lock()
	sp.playing = ""
	sp.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	select {
	case <-got:
		t.Fatal("continued a second stop")
	default:
	}
}

func TestAPausedSongIsLeftAlone(t *testing.T) {
	e, sp, _ := setup(t)
	e.timing.Look, e.timing.LookClose, e.timing.CutIn = 10*time.Millisecond, 10*time.Millisecond, 100*time.Millisecond
	r := req(sp)
	r.Track.Length = time.Hour
	r.Track.Resume = func(context.Context, time.Duration) (Track, time.Duration, error) {
		t.Error("continued by itself")
		return Track{}, 0, errors.New("no")
	}
	e.Start(context.Background(), r)
	time.Sleep(700 * time.Millisecond)
	sp.mu.Lock()
	sp.playing = ""
	sp.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
}

func TestLostMuteAnswerStillRestores(t *testing.T) {
	e, sp, _ := setup(t)
	sp.volHook = func(v int) error {
		if v == 0 {
			return context.DeadlineExceeded
		}
		return nil
	}
	e.Run(context.Background(), req(sp))
	if sp.volume != 40 {
		t.Fatalf("left at %d", sp.volume)
	}
}

func TestFailedRestoreIsMadeUpDuringTheSong(t *testing.T) {
	e, sp, st := setup(t)
	fails := 3
	sp.volHook = func(v int) error {
		if v > 0 && fails > 0 {
			fails--
			sp.volume = 0
			return errors.New("lost")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx, req(sp)); close(done) }()
	defer func() { cancel(); <-done }()
	for i := 0; ; i++ {
		e.recoverOnce(context.Background())
		all, _ := st.PendingVolumes(context.Background())
		sp.mu.Lock()
		ok := sp.volume == 40 && fails == 0 && len(all) == 0
		sp.mu.Unlock()
		if ok {
			break
		}
		if i > 300 {
			t.Fatal("still muted")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRecoveryLeavesHeldSpeakersAlone(t *testing.T) {
	e, sp, st := setup(t)
	sp.volume = 0
	st.SetPendingVolume(context.Background(), "d1", 55, time.Now())
	done := e.hold("d1")
	e.recoverOnce(context.Background())
	if sp.volume != 0 {
		t.Fatal("unmuted under a run")
	}
	done()
	e.recoverOnce(context.Background())
	if sp.volume != 55 {
		t.Fatal(sp.volume)
	}
}

func TestSupersededRunDoesNotUnmuteInBetween(t *testing.T) {
	e, sp, st := setup(t)
	ctx := context.Background()
	if _, err := e.Start(ctx, req(sp)); err != nil {
		t.Fatal(err)
	}
	wait := func(what string, f func() bool) {
		t.Helper()
		for i := 0; i < 500; i++ {
			sp.mu.Lock()
			ok := f()
			sp.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatal(what)
	}
	wait("mute", func() bool { return len(sp.volumes) > 0 })
	e.Start(ctx, req(sp))
	wait("restore", func() bool { return sp.volume == 40 })
	for i, v := range sp.volumes[:len(sp.volumes)-1] {
		if v != 0 {
			t.Fatalf("unmuted in between: %d %v", i, sp.volumes)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if all, _ := st.PendingVolumes(ctx); len(all) != 0 {
		t.Fatal(all)
	}
}

func TestPushIsRepeatedWhenTheSpeakerNeverComes(t *testing.T) {
	e, sp, _ := setup(t)
	sp.fetchOK = false
	n := 0
	sp.playHook = func() {
		if n++; n == 2 {
			sp.fetchOK = true
		}
	}
	// the first push is lost: nothing plays afterwards
	lost := true
	e.sp = prompt{lossy{sp, &lost}}
	run, _ := e.Run(context.Background(), req(sp))
	if run.Outcome != store.OutcomeConfirmed || !strings.Contains(steps(run), StepReplay) {
		t.Fatalf("%s %s", run.Outcome, steps(run))
	}
}

func TestPushIsRetriedAfterAnError(t *testing.T) {
	e, sp, _ := setup(t)
	sp.playErr = errors.New("connection reset")
	sp.playHook = func() {
		if strings.Count(strings.Join(sp.calls, ","), "play") > 1 {
			sp.playErr = nil
		}
	}
	run, _ := e.Run(context.Background(), req(sp))
	if run.Outcome != store.OutcomeConfirmed {
		t.Fatalf("%s %s", run.Outcome, steps(run))
	}
}

// lossy accepts the first push without playing it.
type lossy struct {
	*fakeSpeaker
	lost *bool
}

func (l lossy) PlayURL(ctx context.Context, a, b, c string) error {
	err := l.fakeSpeaker.PlayURL(ctx, a, b, c)
	if *l.lost {
		*l.lost = false
		l.mu.Lock()
		l.playing = ""
		l.mu.Unlock()
	}
	return err
}

func TestPanicInARunLeavesNothingMutedOrBusy(t *testing.T) {
	e, sp, _ := setup(t)
	sp.playHook = func() { sp.playHook = nil; sp.mu.Unlock(); defer sp.mu.Lock(); panic("boom") }
	e.Start(context.Background(), req(sp))
	for i := 0; i < 300 && (e.Playing("d1") || e.held("d1")); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if e.Playing("d1") || e.held("d1") || sp.volume != 40 {
		t.Fatalf("playing %v volume %d", e.Playing("d1"), sp.volume)
	}
}

func TestHandoffStopsOnceWithoutMute(t *testing.T) {
	e, sp, _ := setup(t)
	rq := req(sp)
	rq.Handoff = true
	run, err := e.Run(context.Background(), rq)
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != store.OutcomeConfirmed {
		t.Fatalf("outcome %s %s: %s", run.Outcome, run.Detail, steps(run))
	}
	if got := steps(run); got != "stop1,play,fetched,confirmed" {
		t.Fatalf("steps %s", got)
	}
	if len(sp.volumes) != 0 {
		t.Fatalf("a handoff touched the volume: %v", sp.volumes)
	}
}

// The speaker's own pause key: no record, and while paused the speaker hangs up our stream.
// Its play key plays on from what it still holds: that is our song, not something else (#26).
func TestPauseKeyThenPlayKeyGoesOn(t *testing.T) {
	e, sp, st := setup(t)
	e.timing.Look, e.timing.LookClose, e.timing.CutIn = 10*time.Millisecond, 10*time.Millisecond, 100*time.Millisecond
	r := req(sp)
	r.Track.Length = time.Hour
	var mu sync.Mutex
	lost := false
	r.Track.Lost = func() bool { mu.Lock(); defer mu.Unlock(); return lost }
	run, err := e.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond) // past the guard and the cut-in window
	sp.mu.Lock()
	was := sp.playing
	sp.playing = ""
	sp.mu.Unlock()
	mu.Lock()
	lost = true
	mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	sp.mu.Lock()
	sp.playing = was
	sp.mu.Unlock()
	time.Sleep(400 * time.Millisecond)
	saved, _ := st.Run(context.Background(), run.ID)
	if s := steps(saved); !strings.Contains(s, "paused,continued") || strings.Contains(s, "stopped") {
		t.Fatalf("steps %s", s)
	}
}
