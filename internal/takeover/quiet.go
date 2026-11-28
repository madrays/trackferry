package takeover

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"trackferry/internal/xiaomi"
)

// How a speaker is kept from being heard while Xiao AI answers. Models differ in what they
// obey, so it is set per speaker.
const (
	HushNormal = ""       // volume to nothing, and stop
	HushStrong = "strong" // also mute and pause through Mi Home
	HushWait   = "wait"   // as strong where that is possible, and the song waits until she is done
)

// HushChosenNormal is how the choice of HushNormal is stored: nothing stored leaves it to the model.
const HushChosenNormal = "normal"

// HushStyle is what a stored choice means for a speaker.
func HushStyle(stored, hardware, did string) string {
	switch {
	case stored == HushChosenNormal, stored != HushWait && !xiaomi.CanHush(hardware, did):
		return HushNormal
	case stored == "" && xiaomi.Stubborn(hardware):
		return HushStrong
	}
	return stored
}

// deepTimeout bounds one Mi Home call: the song waits for it.
const deepTimeout = 3 * time.Second

func (e *Engine) device(dev string) *device {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.devs[dev]
	if d == nil {
		d = &device{}
		e.devs[dev] = d
	}
	return d
}

func (e *Engine) style(dev string) string {
	if e.Style == nil || e.Deep == nil {
		return HushNormal
	}
	return e.Style(dev)
}

// muting is what became of an attempt to mute.
type muting struct {
	volume    int  // to come back to; -1: not known
	inherited bool // somebody else had muted it
	muted     bool
	status    string
	statusOK  bool
	err       error // of setting the volume
	note      string
}

// mute turns a speaker's volume to nothing and remembers what to come back to. Whoever comes
// second waits for the first and finds its work done.
func (e *Engine) mute(ctx context.Context, dev string) (m muting) {
	d := e.device(dev)
	d.vol.Lock()
	defer d.vol.Unlock()
	bg := context.WithoutCancel(ctx)
	m.volume = -1
	if v, ok, err := e.st.PendingVolume(bg, dev); err == nil && ok {
		m.volume, m.inherited, m.muted = v, true, true
		m.statusOK, m.status = true, fmt.Sprintf("沿用待恢复音量 %d", v)
		e.deepen(dev, d)
		m.err = e.sp.SetVolume(ctx, dev, 0)
		return m
	}
	known := e.volume(dev)
	type answer struct {
		p   xiaomi.PlayStatus
		err error
	}
	ask := make(chan answer, 1)
	go func() {
		p, err := e.sp.PlayStatus(ctx, dev)
		ask <- answer{p, err}
	}()
	if known > 0 {
		m.volume = known
	} else {
		a := <-ask
		if a.err != nil {
			m.status = describe(a.err) + "，不静音"
			return m
		}
		e.learn(dev, a.p.Volume)
		m.volume = max(a.p.Volume, e.volume(dev))
		m.statusOK, m.status = true, fmt.Sprintf("状态 %d，音量 %d", a.p.Status, a.p.Volume)
	}
	if m.volume <= 0 {
		return m
	}
	if err := e.st.SetPendingVolume(bg, dev, m.volume, e.now()); err != nil {
		m.note = "记不下原音量，不静音"
		return m
	}
	var early *answer // given before the mute was sent: it is the volume as it was
	if known > 0 {
		select {
		case a := <-ask:
			early = &a
		default:
		}
	}
	e.deepen(dev, d)
	m.err = e.sp.SetVolume(ctx, dev, 0)
	// Only a plain refusal means nothing changed. A call that got lost may have arrived.
	if m.muted = m.err == nil || !refused(m.err); !m.muted {
		e.undeepen(dev)
		e.st.ClearPendingVolume(bg, dev)
	}
	if known > 0 {
		// The answer tells the volume only if it was given before the mute arrived: what it
		// says afterwards is the least the speaker goes down to, which need not be 0, or a
		// step of its fade on the way there. Such an answer is only believed when louder than
		// the volume known: a mute never raises it. A lower one would only be meant if the
		// volume was turned down in the half minute since it was last read (Refresh), which
		// is far rarer than a song coming back at a step of the fade.
		var a answer
		if early != nil {
			a = *early
		} else {
			a = <-ask
		}
		if m.statusOK = a.err == nil; !m.statusOK {
			m.status = fmt.Sprintf("%s，按上次的音量 %d", describe(a.err), known)
			return m
		}
		if a.p.Volume > e.floor(dev) && a.p.Volume != known && (early != nil || a.p.Volume > known) {
			m.volume = a.p.Volume
			e.know(dev, m.volume)
			if m.muted {
				e.st.ChangePendingVolume(bg, dev, m.volume)
			}
		}
		m.status = fmt.Sprintf("状态 %d，音量 %d", a.p.Status, m.volume)
	}
	return m
}

// deepen mutes through Mi Home as well, without waiting for it.
func (e *Engine) deepen(dev string, d *device) {
	if e.style(dev) == HushNormal {
		return
	}
	d.deep.Lock()
	d.deepKnown = true
	go func() {
		defer d.deep.Unlock()
		c, cancel := context.WithTimeout(context.Background(), deepTimeout)
		defer cancel()
		err := e.Deep(c, dev, true)
		if err == nil || !refused(err) {
			d.deepOn = true
		}
		if err != nil {
			e.log.Warn("接管　米家静音没有成功", "原因", describe(err))
		}
	}()
}

// deepened ends a Mi Home mute before our song is pushed, waiting for one on its way. Neither
// its pause nor its end may reach the song: LX06 was seen to drop the stream within 20 ms of
// the mute being lifted, while claiming to play. The volume stays at nothing.
func (e *Engine) deepened(dev string) {
	for range 2 {
		if _, err := e.undeepen(dev); err == nil {
			return
		} else {
			e.log.Warn("接管　米家静音没能在放歌前解除", "原因", describe(err))
		}
	}
}

// undeepen takes the mute back, after one that is still on its way.
func (e *Engine) undeepen(dev string) (lifted bool, err error) {
	if e.Deep == nil {
		return false, nil
	}
	d := e.device(dev)
	d.deep.Lock()
	defer d.deep.Unlock()
	if !d.deepOn {
		return false, nil
	}
	c, cancel := context.WithTimeout(context.Background(), deepTimeout)
	defer cancel()
	if err = e.Deep(c, dev, false); err == nil {
		d.deepOn = false
	}
	return err == nil, err
}

// Unhush lifts a mute that an earlier setting, or an earlier process, may have left.
func (e *Engine) Unhush(ctx context.Context, dev string) error {
	if e.Deep == nil {
		return nil
	}
	d := e.device(dev)
	d.deep.Lock()
	defer d.deep.Unlock()
	if d.deepKnown { // deepOn tells, and a song may be playing
		return nil
	}
	d.deepKnown = true
	c, cancel := context.WithTimeout(ctx, 2*deepTimeout)
	defer cancel()
	return e.Deep(c, dev, false)
}

// halt pauses and stops at once; some models ignore a lone stop.
func (e *Engine) halt(ctx context.Context, dev string) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.sp.PlayerOp(ctx, dev, "pause")
	}()
	err := e.sp.PlayerOp(ctx, dev, "stop")
	wg.Wait()
	return err
}

// Quiet mutes a speaker while it is still being decided who answers a request, because
// deciding can take longer than Xiao AI needs to start talking. The returned function ends
// it: with back the volume returns, otherwise the run that was just started looks after it.
func (e *Engine) Quiet(dev string) func(back bool) {
	letGo := e.hold(dev)
	d := e.device(dev)
	e.mu.Lock()
	d.quiets++
	gen := d.gen
	e.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m := e.mute(context.Background(), dev)
		if m.muted && !m.inherited {
			e.mu.Lock()
			d.quieted = m.volume
			e.mu.Unlock()
		}
	}()
	return func(back bool) {
		go func() {
			<-done
			defer letGo()
			e.mu.Lock()
			d.quiets--
			mine, v := d.gen == gen && d.quiets == 0 && d.quieted > 0, d.quieted
			if d.quiets == 0 || d.gen != gen {
				d.quieted = 0
			}
			e.mu.Unlock()
			if back && mine {
				e.putBack(dev, v)
			}
		}()
	}
}

// errStopped is why a run ends when the listener stops it.
var errStopped = errors.New("stopped by the listener")
