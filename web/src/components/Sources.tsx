import { useCallback, useEffect, useState } from 'react';
import { api, ApiError, type CacheStatus, type FillStatus, type Quality, type SourceHealth } from '../api';
import { clock } from '../copy';
import { Pick } from './Pick';
import { Setting, WavesIcon, type Light } from './Setting';
import { Netease } from './Netease';
import { SourceIcon } from './SourceIcon';
import { ms, stopLabel } from '../route';

function size(bytes: number) {
  const mb = bytes / 2 ** 20;
  return mb < 1024 ? `${Math.round(mb)} MB` : `${+(mb / 1024).toFixed(1)} GB`;
}

/** The cache's limit that is none. */
const NO_LIMIT = -1;

export function Sources({ onLight }: { onLight: (l: Light) => void }) {
  const [sources, setSources] = useState<SourceHealth[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [quality, setQuality] = useState<Quality | null>(null);
  const [fill, setFill] = useState<FillStatus | null>(null);
  const load = useCallback(() => api.library().then((r) => { setSources(r.sources ?? []); setQuality(r.quality ?? null); setFill(r.fill ?? null); }).catch((e) => setError((e as ApiError).message)), []);
  const pickQuality = async (q: Quality) => {
    try {
      setQuality((await api.setQuality(q)).quality);
      setError(null);
    } catch (e) {
      setError((e as ApiError).message);
    }
  };
  useEffect(() => { load(); }, [load]);
  const set = async (s: SourceHealth, on: boolean) => {
    try {
      setSources((await api.setSource(s.source, on)).sources);
      setError(null);
    } catch (e) {
      setError((e as ApiError).message);
    }
  };
  const [cache, setCache] = useState<CacheStatus | null>(null);
  const [clearing, setClearing] = useState(false);
  useEffect(() => { api.cache().then(setCache).catch(() => {}); }, []); // not there: nothing to show
  const change = async (by: Promise<CacheStatus>) => {
    try {
      setCache(await by);
      setClearing(false);
      setError(null);
    } catch (e) {
      setError((e as ApiError).message);
    }
  };
  const limit = (mb: number) => change(api.setCache(mb));
  const on = sources.filter((s) => s.enabled).length;
  const down = (s: SourceHealth) => s.enabled && (s.open_until_ms ?? 0) > Date.now();
  const light: Light | null = !sources.length ? null : sources.some(down) ? 'warn' : on ? 'ok' : 'off';
  useEffect(() => { if (light) onLight(light); }, [light, onLight]);
  return (
    <Setting id="sources" icon={<WavesIcon />} title="线上音源" says="曲库没有时一起问，只放能完整听的" light={light ?? 'off'} state={sources.length ? on ? `启用 ${on} 个` : '都关着' : undefined}>
      <ul className="sources">
        {sources.map((s, i) => {
          const all = s.ok + s.failed;
          return (
            <li key={s.source} className={s.enabled ? '' : 'off'}>
              <span className="source-no num">{i + 1}</span>
              <SourceIcon name={s.source} />
              <span className="source-state"><b>{stopLabel[s.source]}</b>{s.source === 'netease' && s.enabled && !down(s) && !s.last_error ? <Netease /> : <small>{!s.enabled ? '已关闭' : down(s) ? <span className="bad">连续失败，{clock(s.open_until_ms!).slice(0, 5)} 后再试</span> : s.last_at_ms ? `上次 ${clock(s.last_at_ms).slice(0, 5)}` : '启动后还没用过'}{s.last_error && s.enabled ? `，${s.last_error}` : ''}</small>}</span>
              <span className="source-time num">{s.enabled && s.last_at_ms ? ms(s.last_ms) : ''}</span>
              <span className="source-rate">{all > 0 && <><i><i style={{ width: `${s.ok / all * 100}%` }} /></i><small className="num">{s.ok} / {all}</small></>}</span>
              <label className="switch"><input type="checkbox" role="switch" checked={s.enabled} onChange={(e) => set(s, e.target.checked)} aria-label={`使用${stopLabel[s.source]}`} /><span /></label>
            </li>
          );
        })}
      </ul>
      {quality && (
        <div className="kept">
          <div><b>在线音质</b><small>{quality === 'high' ? '能要到多高就要多高：登录了会员的网易云可到 320k；酷我、酷狗、咪咕的免费接口目前只给 128k MP3，选这项也一样' : '一律要 128k，省流量、起播快'}。每首歌真实的格式和码率显示在“此刻”页，点一下能换别的版本。曲库里的歌始终是原文件</small></div>
          <Pick label="在线音质" value={quality} onPick={pickQuality}
            choices={[{ value: 'high', label: '要最高' }, { value: 'standard', label: '省流量' }]} />
        </div>
      )}
      {fill && (
        <div className="kept">
          <div><b>补全封面和歌词</b><small>{fill.on
            ? `曲库里没有封面的专辑、没有歌词的歌，到开着的平台上找同名同歌手、时长相近的那一首${fill.running ? `；正在找，还剩 ${fill.left.toLocaleString('zh-CN')} 张专辑，已补上 ${fill.found.toLocaleString('zh-CN')} 首的封面` : fill.missing ? `；还有 ${fill.missing.toLocaleString('zh-CN')} 首没有封面，每 6 小时再找一轮，找过没有的 30 天后再找` : '；曲库的歌都有封面了'}。歌词在播放时才找，找到就留着。平台一个一个问，每家每秒最多一两次，不会因此被封`
            : '关着：没有封面的专辑显示占位图，没有歌词的歌不显示歌词'}</small></div>
          <Pick label="补全封面和歌词" value={fill.on ? 'on' : 'off'} onPick={async (v) => {
            try { setFill((await api.setFill(v === 'on')).fill); } catch (e) { setError((e as ApiError).message); }
          }} choices={[{ value: 'on', label: '开' }, { value: 'off', label: '关' }]} />
        </div>
      )}
      {cache && (
        <div className="kept">
          <div><b>听过的歌留一份</b><small>{cache.limit_mb ? <>已存 <span className="num">{cache.songs}</span> 首，{size(cache.bytes)}。{cache.limit_mb === NO_LIMIT ? '不会删，磁盘只剩 1 GB 时不再存新的。' : '没存满不会删，满了先删最久没听的。'}{cache.mounted ? '放在挂载的缓存盘' : '放在 data/cache'}</> : '已关闭，每次都从线上取'}</small></div>
          <Pick label="缓存上限" value={cache.limit_mb} onPick={limit}
            choices={[...[...new Set([0, 1024, 2048, 5120, 10240, 20480, 51200, Math.max(0, cache.limit_mb)])].sort((a, b) => a - b), NO_LIMIT].map((m) => ({ value: m, label: m === NO_LIMIT ? '不限' : m ? `最多 ${size(m * 2 ** 20)}` : '关闭' }))} />
          {cache.songs > 0 && (clearing
            ? <span className="kept-sure"><button className="btn btn-small btn-danger" onClick={() => change(api.clearCache())}>清空</button><button className="btn btn-small btn-quiet" onClick={() => setClearing(false)}>取消</button></span>
            : <button className="btn btn-small btn-quiet" onClick={() => setClearing(true)}>清空</button>)}
        </div>
      )}
      {cache && cache.limit_mb !== 0 && <p className="hint cache-note">缓存只为自家音箱少走一趟网络，存下的歌请只在家里自己听，不要拷出去传播或用作他途。登录网易云后靠会员放的歌也会留一份，会员到期后这些缓存仍在本地，是否继续保留请自行斟酌。音乐版权归词曲作者、唱片公司和平台所有，请尊重版权、支持正版。</p>}
      {error && <p className="field-error">{error}</p>}
    </Setting>
  );
}
