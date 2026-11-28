import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import { api, type Device } from '../api';
import { HERE } from '../ear';
import { here, useHere } from '../here';
import { useStage } from '../outputs';
import { href, type Page, type Place } from '../nav';
import { Cover } from './Cover';
import { SwitchOutput } from './SwitchOutput';
import { Jelly } from './jelly';
import { commandTV, tvID, useTVOutputs } from '../tv';
import { onDeck, useLive } from '../live';

type Nav = (p: Place) => void;
const draw = (d: string) => <svg viewBox="0 0 24 24" width="25" height="25" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={d} /></svg>;
const items: { page: Page; label: string; icon: ReactNode }[] = [
  { page: 'home', label: '此刻', icon: draw('M3 14.5c2-1.6 4-1.6 6 0s4 1.6 6 0 4-1.6 6 0M3 18.5c2-1.6 4-1.6 6 0s4 1.6 6 0 4-1.6 6 0M9.5 10.8V4.2l6 1.6v2.6l-6-1.6') },
  { page: 'library', label: '曲库', icon: draw('M4 4.5v15M8.5 4.5v15M13 5.2l3.4-.9 4 14.3-3.4 1z') },
  { page: 'more', label: '更多', icon: draw('M5.5 5.5h4v4h-4zM14.5 5.5h4v4h-4zM5.5 14.5h4v4h-4zM14.5 14.5h4v4h-4z') },
];
/** The tab a page belongs to. */
const tabOf = (p: Page): Page => (p === 'flow' || p === 'say' || p === 'speakers' || p === 'account' || p === 'tv' ? 'more' : p);

/** What is on, and the two buttons wanted most, on every page but 此刻. */
function NowBar({ devices, navigate, wide }: { devices: Device[]; navigate: Nav; wide?: boolean }) {
  const h = useHere();
  const { flows } = useLive();
  // The same output as 此刻: the pick when it has a song, else one that plays (see output-rule.ts).
  const { shown, bar: d, pick } = useStage(devices);
  const [busy, setBusy] = useState(false);
  const televisions = useTVOutputs();
  if (!d) return null;
  const press = async (a: 'play' | 'pause' | 'next') => {
    if (d.id === HERE) { if (a === 'next') here.next(); else here.toggle(); return; }
    setBusy(true);
    const tv = televisions.find((item) => item.id === tvID(d.id));
    await (tv ? commandTV(tv.id, { action: a, revision: tv.playback.revision, entry: tv.playback.entry }) : api.press(d.id, a)).catch(() => {});
    setBusy(false);
  };
  const tvOf = televisions.find((item) => item.id === tvID(d.id));
  const level = d.id === HERE ? (h.canVolume ? h.volume : null) : tvOf ? tvOf.playback.volume ?? null : onDeck(flows, d.id)?.player?.volume ?? null;
  const turn = (v: number) => {
    if (d.id === HERE) here.volume(v);
    else if (tvOf) commandTV(tvOf.id, { action: 'volume', volume: v, revision: tvOf.playback.revision, entry: tvOf.playback.entry }).catch(() => {});
    else api.press(d.id, 'volume', { volume: v }).catch(() => {});
  };
  return (
    <div className="nowbar glass dock-glass">
      <button type="button" className="nowbar-song" onClick={() => { if (shown !== d.id) pick(d.id); navigate({ page: 'home' }); }} aria-label={`正在放${d.title}，打开播放器`}>
        <Cover cover={d.cover} title={d.title ?? ''} artist={d.artist} /><span><b>{d.title}</b><small>{d.waiting && d.id !== HERE ? `正在送到${d.name}` : d.name}</small></span>
      </button>
      <SwitchOutput key={d.id} source={d.id} devices={devices} className="nowbar-btn nowbar-switch" />
      <button type="button" className="nowbar-btn" disabled={busy} onClick={() => press(d.playing ? 'pause' : 'play')} aria-label={d.playing ? '暂停' : '播放'}>{d.waiting ? <span className="spinner spinner-tiny" /> : <svg viewBox="0 0 24 24" width="24" height="24" fill="currentColor" aria-hidden="true"><path d={d.playing ? 'M7 5h3.6v14H7zM13.4 5H17v14h-3.6z' : 'M8 5.2v13.6L19 12z'} /></svg>}</button>
      <button type="button" className="nowbar-btn nowbar-next" disabled={busy || (d.id === HERE && h.songs.length < 2)} onClick={() => press('next')} aria-label="下一首"><svg viewBox="0 0 24 24" width="24" height="24" fill="currentColor" aria-hidden="true"><path d="M4 6v12l8-6zM12 6v12l8-6z" /></svg></button>
      {wide && level !== null && level >= 0 && <Level key={d.id} value={level} onSet={turn} />}
    </div>
  );
}

/** The volume on the wide bar: sent when let go, so a drag is one change for the speaker. */
function Level({ value, onSet }: { value: number; onSet: (v: number) => void }) {
  const [v, setV] = useState(value);
  const held = useRef(false);
  useEffect(() => { if (!held.current) setV(value); }, [value]);
  const send = () => { held.current = false; if (v !== value) onSet(v); };
  return (
    <label className="nowbar-level" title={`音量 ${v}`}>
      <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M4 9.5h3.5L12 5.5v13l-4.5-4H4zM15.5 9a4 4 0 0 1 0 6M18 6.5a7.5 7.5 0 0 1 0 11" /></svg>
      <input type="range" min={0} max={100} value={v} aria-label="音量" onPointerDown={() => { held.current = true; }} onChange={(e) => setV(Number(e.target.value))} onPointerUp={send} onKeyUp={send} onBlur={() => { if (held.current) send(); }} />
      <span className="num">{v}</span>
    </label>
  );
}

/** The bar of a wide screen: what is on, on every page but 此刻, so it can be paused anywhere. */
export function DeskBar({ page, devices, navigate }: { page: Page; devices: Device[]; navigate: Nav }) {
  if (page === 'home' || page === 'welcome') return null;
  return <div className="deskbar"><NowBar devices={devices} navigate={navigate} wide /></div>;
}

/** The bar of a phone: a capsule of glass whose lens follows the finger, and search on its own. */
export function Dock({ page, devices, navigate, dot }: { page: Page; devices: Device[]; navigate: Nav; dot: boolean }) {
  const tab = tabOf(page);
  const at = items.findIndex((t) => t.page === tab);
  const bar = useRef<HTMLElement>(null);
  const lens = useRef<HTMLSpanElement>(null);
  const [small, setSmall] = useState(false);
  const [over, setOver] = useState<number | null>(null); // under the finger
  const grip = useRef<{ id: number; x: number; y: number; moved: boolean } | null>(null);
  const skip = useRef(false);
  const jelly = useRef<Jelly>(null);
  const folded = small && tab !== 'home' && tab !== 'search';

  useEffect(() => {
    setSmall(false);
    let last = Math.max(0, scrollY), run = 0;
    document.documentElement.classList.toggle('under', last > 8);
    const f = () => {
      const y = Math.max(0, scrollY), d = y - last;
      document.documentElement.classList.toggle('under', y > 8);
      last = y;
      run = Math.sign(d) === Math.sign(run) ? run + d : d;
      if (y < 40 || run < -24) setSmall(false);
      else if (run > 36) setSmall(true);
    };
    addEventListener('scroll', f, { passive: true });
    return () => removeEventListener('scroll', f);
  }, [page]);

  const shown = over ?? at;
  useLayoutEffect(() => {
    const b = bar.current!;
    const j = jelly.current = new Jelly(b, lens.current!, () => [...b.querySelectorAll('a')]);
    return () => j.stop();
  }, []);
  useLayoutEffect(() => {
    const b = bar.current!, l = lens.current!, j = jelly.current!;
    if (grip.current?.moved) return;
    let first = true;
    const set = () => {
      const a = b.querySelectorAll('a')[shown] as HTMLElement | undefined;
      const was = l.style.opacity === '1';
      l.style.opacity = a ? '1' : '0';
      if (a) { if (first && was) j.go(a.offsetLeft, a.offsetWidth); else j.put(a.offsetLeft, a.offsetWidth); }
      first = false;
    };
    set();
    let skipOne = true; // an observer calls at once
    const ro = new ResizeObserver(() => { if (skipOne) { skipOne = false; return; } set(); });
    ro.observe(b);
    return () => ro.disconnect();
  }, [shown, folded]);

  const nearest = (x: number) => {
    const as = [...bar.current!.querySelectorAll('a')];
    let best = 0;
    as.forEach((a, i) => { const r = a.getBoundingClientRect(), s = as[best].getBoundingClientRect(); if (Math.abs(x - r.left - r.width / 2) < Math.abs(x - s.left - s.width / 2)) best = i; });
    return best;
  };
  const down = (e: React.PointerEvent) => {
    skip.current = false;
    if (folded || e.button) return;
    grip.current = { id: e.pointerId, x: e.clientX, y: e.clientY, moved: false };
    bar.current!.classList.add('held');
    jelly.current!.hold(true);
    setOver(nearest(e.clientX)); // it comes to the finger
  };
  const move = (e: React.PointerEvent) => {
    const g = grip.current, b = bar.current!, l = lens.current!;
    if (g?.id !== e.pointerId) return;
    if (!g.moved && Math.hypot(e.clientX - g.x, e.clientY - g.y) < 6) return;
    if (!g.moved) { g.moved = true; b.setPointerCapture(e.pointerId); l.style.opacity = '1'; }
    const r = b.getBoundingClientRect(), w = b.querySelector('a')!.offsetWidth, pad = 4, x = e.clientX - r.left - w / 2, end = b.offsetWidth - w - pad;
    jelly.current!.go(Math.min(end, Math.max(pad, x)), w);
    jelly.current!.drag(x < pad ? x - pad : x > end ? x - end : 0, e.clientY - g.y);
    setOver(nearest(e.clientX));
  };
  const up = (e: React.PointerEvent) => {
    const g = grip.current;
    if (g?.id !== e.pointerId) return;
    grip.current = null;
    bar.current!.classList.remove('held');
    jelly.current!.hold(false);
    skip.current = true;
    const i = nearest(e.clientX);
    setOver(null);
    const go = e.type === 'pointerup' && items[i].page !== page;
    const a = bar.current!.querySelectorAll('a')[go ? i : at] as HTMLElement | undefined;
    if (a) jelly.current!.go(a.offsetLeft, a.offsetWidth);
    else lens.current!.style.opacity = '0';
    if (go) navigate({ page: items[i].page });
    else if (e.type === 'pointerup' && !g.moved && items[i].page === page) scrollTo({ top: 0, behavior: 'smooth' });
  };

  return (
    <div className={`dock${folded ? ' folded' : ''}`}>
      {page !== 'home' && <NowBar devices={devices} navigate={navigate} />}
      <nav ref={bar} className="capsule glass dock-glass" aria-label="主导航" onPointerDown={down} onPointerMove={move} onPointerUp={up} onPointerCancel={up}>
        <span ref={lens} className="lens" aria-hidden="true"><i /><i /></span>
        {items.map((t, i) => (
          <a key={t.page} href={href({ page: t.page })} draggable={false} className={shown === i ? 'on' : ''} aria-current={tab === t.page ? 'page' : undefined}
            aria-label={folded ? t.label : undefined} tabIndex={folded && tab !== t.page ? -1 : undefined}
            onClick={(e) => {
              e.preventDefault();
              if (skip.current) { skip.current = false; return; }
              if (folded) { setSmall(false); return; }
              if (page === t.page) scrollTo({ top: 0, behavior: 'smooth' });
              navigate({ page: t.page });
            }}>
            {t.icon}<span>{t.label}</span>{t.page === 'more' && dot && <i className="dot" aria-label="需要处理" />}
          </a>
        ))}
      </nav>
      <a className={`orb glass dock-glass ${tab === 'search' ? 'on' : ''}`} href="#/search" aria-label="搜索" aria-current={tab === 'search' ? 'page' : undefined} onClick={(e) => { e.preventDefault(); navigate({ page: 'search' }); }}>{draw('M10.8 4.2a6.6 6.6 0 1 1 0 13.2 6.6 6.6 0 0 1 0-13.2zM15.7 15.7l4.6 4.6')}</a>
    </div>
  );
}
