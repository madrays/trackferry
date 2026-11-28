import { useEffect, useState } from 'react';
import { api, ApiError, type Listening as Prefs } from '../api';
import { Pick } from './Pick';
import { Setting } from './Setting';

const busy = [1000, 1500, 2000, 3000, 4000, 5000, 8000, 10000];
const quiet = [4000, 6000, 10000, 15000, 20000, 30000];
const secs = (ms: number) => `${ms / 1000} 秒`;

function EarIcon() {
  return <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M7 9.5a5 5 0 1 1 9.2 2.7c-.9 1.4-2.2 2.1-2.2 3.8a2.7 2.7 0 0 1-5.2.9M9.6 9.6a2.4 2.4 0 1 1 4 1.8" /></svg>;
}

/** How often Xiaomi is asked what the speakers heard: more often hears sooner, and asks more of an account Xiaomi may limit. */
export function Listening() {
  const [p, setP] = useState<Prefs | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => { api.listening().then(setP).catch((e) => setError((e as ApiError).message)); }, []);
  const set = async (next: Partial<Prefs>) => {
    const was = p;
    if (p) setP({ ...p, ...next });
    try {
      setP(await api.setListening(next));
      setError(null);
    } catch (e) {
      setP(was);
      setError((e as ApiError).message);
    }
  };
  const options = (list: number[], have: number, from = 0) => [...new Set([...list, have])].filter((ms) => ms >= from).sort((a, b) => a - b).map((ms) => ({ value: ms, label: secs(ms) }));
  return (
    <Setting id="listen" icon={<EarIcon />} title="听歌识别" state={p ? (p.listen ? '开' : '关') : undefined} light={p && !p.listen ? 'warn' : undefined}
      says="多久问一次小米音箱听到了什么。说“小爱同学，放晴天”靠的就是它">
      {p && <>
        <div className="kept">
          <div><b>听音箱说的话</b><small>关掉后不再读对话记录，不会被小米限流，但说话点歌、“下一首”“换个版本”都没反应；从网页、电视、Home Assistant 往音箱送歌照常</small></div>
          <label className="switch"><input type="checkbox" role="switch" checked={p.listen} onChange={(e) => set({ listen: e.target.checked })} aria-label="听音箱说的话" /><span /></label>
        </div>
        {p.listen && <>
          <div className="kept">
            <div><b>多久读一次</b><small>接管的音箱按这个间隔读，观察的音箱慢一倍。间隔越短，说完到出歌越快（平均多等半个间隔）；但每台音箱每次都是一次小米云端请求，音箱多、间隔短，容易被小米限流，表现为“读取失败”、点歌没反应。默认 2 秒；常被限流就调长，只有一两台又想更快可以调短</small></div>
            <Pick label="多久读一次" value={p.interval_ms} onPick={(ms) => set({ interval_ms: ms, idle_ms: Math.max(p.idle_ms, ms) })} choices={options(busy, p.interval_ms)} />
          </div>
          <div className="kept">
            <div><b>没人说话时放慢</b><small>一台音箱 {p.quiet_min} 分钟没听到任何话，就改成按下面的间隔读；一听到话（哪怕不是点歌）马上回到上面的间隔，并保持 {p.quiet_min} 分钟。刚点过歌、还在等“换个版本”这类口令的音箱不放慢。放慢时，第一句点歌要多等一会儿</small></div>
            <label className="switch"><input type="checkbox" role="switch" checked={p.idle} onChange={(e) => set({ idle: e.target.checked })} aria-label="没人说话时放慢" /><span /></label>
          </div>
          {p.idle && <div className="kept">
            <div><b>多久没人说话算安静</b><small>短一些更省请求，但家里说话少时，隔一阵再点歌常赶上放慢；常在固定时段用音箱的，可以调长</small></div>
            <Pick label="多久没人说话算安静" value={p.quiet_min} onPick={(m) => set({ quiet_min: m })} choices={[...new Set([3, 5, 10, 20, 30, 60, p.quiet_min])].sort((a, b) => a - b).map((m) => ({ value: m, label: `${m} 分钟` }))} />
          </div>}
          {p.idle && <div className="kept">
            <div><b>放慢后多久读一次</b></div>
            <Pick label="放慢后多久读一次" value={p.idle_ms} onPick={(ms) => set({ idle_ms: ms })} choices={options(quiet, p.idle_ms, p.interval_ms)} />
          </div>}
          <div className="kept">
            <div><b>被限流时所有音箱一起等</b><small>小米按账号限流：一台读取被拒，整个账号的音箱都停一会儿再试（越拒等得越久，最多 5 分钟），日志只记一次。关掉就各台各自重试，恢复得可能快一点，但更容易一直被拒</small></div>
            <label className="switch"><input type="checkbox" role="switch" checked={p.together} onChange={(e) => set({ together: e.target.checked })} aria-label="被限流时所有音箱一起等" /><span /></label>
          </div>
          <div className="kept">
            <div><b>睡眠时段</b><small>这段时间完全不读，说话点歌没反应，网页送歌照常。按 NAS 的时区算</small></div>
            <label className="switch"><input type="checkbox" role="switch" checked={p.sleep} onChange={(e) => set({ sleep: e.target.checked })} aria-label="睡眠时段" /><span /></label>
          </div>
          {p.sleep && <div className="kept">
            <div><b>从几点到几点</b><small>结束早于开始时跨过零点，例如 23:00 到 07:00</small></div>
            <span className="time-range">
              <input type="time" value={p.sleep_from} aria-label="开始" onChange={(e) => e.target.value && set({ sleep_from: e.target.value })} />
              <span>到</span>
              <input type="time" value={p.sleep_to} aria-label="结束" onChange={(e) => e.target.value && set({ sleep_to: e.target.value })} />
            </span>
          </div>}
        </>}
      </>}
      {error && <p className="field-error" role="alert">{error}</p>}
    </Setting>
  );
}
