package takeover

import (
	"context"
	"testing"
	"time"
)

// OH2P answers 5 once it is muted: that must not become the volume to come back to.
func TestMutedReadingIsNotTheVolume(t *testing.T) {
	e, sp, st := setup(t)
	sp.least, sp.late = 5, true
	e.know("d1", 40)
	for range 3 {
		m := e.mute(context.Background(), "d1")
		if m.volume != 40 || !m.muted {
			t.Fatalf("%+v", m)
		}
		if v, _, _ := st.PendingVolume(context.Background(), "d1"); v != 40 {
			t.Fatal(v)
		}
		e.putBack("d1", m.volume)
		if sp.lastVolume() != 40 || e.volume("d1") != 40 {
			t.Fatal(sp.lastVolume(), e.volume("d1"))
		}
	}
}

func TestLowReadingCountsOnceItStays(t *testing.T) {
	e, _, _ := setup(t)
	now := time.Now()
	e.now = func() time.Time { return now }
	e.learn("d1", 40)
	e.learn("d1", 5)
	if now = now.Add(20 * time.Second); e.volume("d1") != 40 {
		t.Fatal("taken at once")
	}
	e.learn("d1", 30)
	if e.volume("d1") != 30 {
		t.Fatal("an ordinary change waits")
	}
	e.learn("d1", 5)
	now = now.Add(lowStays)
	if e.learn("d1", 5); e.volume("d1") != 5 {
		t.Fatal(e.volume("d1"))
	}
}

func TestRecoversASpeakerLeftAtItsLeast(t *testing.T) {
	e, sp, st := setup(t)
	sp.volume = 5
	st.SetPendingVolume(context.Background(), "d1", 48, time.Now())
	e.recoverOnce(context.Background())
	if sp.lastVolume() != 48 {
		t.Fatal(sp.lastVolume())
	}
}

func TestVolumeThatDoesNotReturnIsSetAgain(t *testing.T) {
	e, sp, _ := setup(t)
	e.checks = []time.Duration{30 * time.Millisecond, 60 * time.Millisecond}
	e.putBack("d1", 40)
	sp.mu.Lock()
	sp.volume = 5
	sp.mu.Unlock()
	until(t, "left at its least", func() bool { sp.mu.Lock(); defer sp.mu.Unlock(); return sp.volume == 40 })
	e.SetVolume(context.Background(), "d1", 5) // meant
	time.Sleep(150 * time.Millisecond)
	if sp.mu.Lock(); sp.volume != 5 {
		t.Error("the listener was overruled")
	}
	sp.mu.Unlock()
}

// A speaker that fades out answers in between once the mute is on its way: 9 of 65 seen on
// a real speaker. Such an answer must not become the volume the song comes back at.
func TestFadingReadingIsNotTheVolume(t *testing.T) {
	e, sp, st := setup(t)
	sp.least, sp.late = 20, true
	e.know("d1", 40)
	m := e.mute(context.Background(), "d1")
	if m.volume != 40 || !m.muted {
		t.Fatalf("%+v", m)
	}
	if v, _, _ := st.PendingVolume(context.Background(), "d1"); v != 40 {
		t.Fatal(v)
	}
}

// Turned up since it was last read: that answer is believed, a mute only lowers.
func TestLouderReadingIsTheVolume(t *testing.T) {
	e, sp, _ := setup(t)
	e.know("d1", 40)
	sp.mu.Lock()
	sp.volume = 60
	sp.mu.Unlock()
	if m := e.mute(context.Background(), "d1"); m.volume != 60 {
		t.Fatalf("%+v", m)
	}
}
