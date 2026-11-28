import { useCallback, useEffect, useState } from 'react';
import { api, ApiError, type FeiniuInfo, type LibraryStatus, type MusicFolder } from '../api';
import { Notice } from './Notice';
import { Pick } from './Pick';
import { Setting, type Light } from './Setting';
import { SourceIcon } from './SourceIcon';
import { clock, day } from '../copy';

/** The mounted music folder: the library when 飞牛音乐 is not connected. */
export function FolderCard({ onLight }: { onLight: (l: Light) => void }) {
  const [lib, setLib] = useState<LibraryStatus | null>(null);
  const [folder, setFolder] = useState<MusicFolder | null>(null);
  const [fn, setFn] = useState<FeiniuInfo | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const [l, f] = await Promise.all([api.library(), api.feiniu()]);
      setLib({ ...l.library, every_minutes: l.every_minutes });
      setFolder(l.folder ?? null);
      setFn(f);
      setError(null);
    } catch (e) {
      setError((e as ApiError).message);
    }
  }, []);
  const running = lib?.running ?? false;
  useEffect(() => {
    load();
    const t = setInterval(() => { if (!document.hidden) load(); }, running ? 1500 : 20000);
    return () => clearInterval(t);
  }, [load, running]);

  const linked = !!fn?.feiniu.linked;
  const missing = !folder || folder.error === 'missing';
  const empty = !missing && folder!.readable && folder!.top_level_entries === 0;
  const last = lib?.last?.source === 'files' ? lib.last : null;
  const report = running && lib?.progress?.source === 'files' ? lib.progress : last;
  const problems = (report?.roots ?? []).reduce((a, r) => a + r.unreadable + (r.error ? 1 : 0), 0);
  const light: Light | null = !lib || !fn ? null
    : linked || missing || empty ? 'off'
    : !folder!.readable ? 'bad'
    : problems > 0 || !folder!.read_only ? 'warn' : 'ok';
  useEffect(() => { if (light) onLight(light); }, [light, onLight]);

  if (!lib || !fn) return <Setting id="folder" icon={<SourceIcon name="folder" />} title="音乐文件夹">{error ? <Notice tone="error" title={error} /> : <span className="spinner" />}</Setting>;
  const n = (v: number) => v.toLocaleString('zh-CN');
  const scan = async () => {
    setBusy(true);
    try { await api.scanLibrary(); await load(); } catch (e) { setError((e as ApiError).message); } finally { setBusy(false); }
  };
  const forget = async () => {
    if (!confirm(`把曲库里的 ${n(lib.tracks)} 首歌清掉？文件夹挂回来后重新扫描会再读进来。`)) return;
    setBusy(true);
    try { await api.forgetEmpty(); await load(); } catch (e) { setError((e as ApiError).message); } finally { setBusy(false); }
  };
  const every = async (m: number) => {
    setLib((x) => x && { ...x, every_minutes: m });
    try { await api.setEvery(m); } catch (e) { setError((e as ApiError).message); }
  };
  const state = linked ? '不使用' : missing ? '没有挂载' : empty ? '是空的' : !folder!.readable ? '读不了' : running ? '正在扫描' : `${n(lib.tracks)} 首`;
  const says = linked ? '已连接飞牛音乐，曲库以飞牛为准，这个文件夹不读'
    : missing || empty ? '不用飞牛音乐时，把音乐文件夹只读挂载进来就是曲库'
    : '标签、内嵌封面、同目录的 cover.jpg 和 .lrc 歌词都会读';

  return (
    <Setting id="folder" icon={<SourceIcon name="folder" />} title="音乐文件夹" light={light ?? 'off'} state={state} says={says}
      actions={!linked && !missing && !empty ? <button className="btn" disabled={running || busy} onClick={scan}>{running ? <><span className="spinner spinner-tiny" />正在扫描</> : '立即扫描'}</button> : undefined}>
      {!linked && (missing || empty) && (
        <div className="howto">
          <p>{missing ? `容器里还没有 ${folder?.path || '/music'}。` : `${folder!.path} 里什么都没有，多半是挂载路径写错了。`}在 compose.yaml 的 volumes 里加一行（改成你的路径，保留末尾的 :ro），再执行 docker compose up -d：</p>
          <pre>{'- /vol1/1000/Music:/music:ro'}</pre>
          <p className="hint">有几个文件夹，就分别挂到 /music 下的不同名字，比如 /vol2/1000/Live:/music/现场:ro。每个会单独统计。</p>
        </div>
      )}
      {!linked && (missing || empty) && lib.tracks > 0 && lib.empty_since ? (
        <Notice tone="warn" title={`曲库里还留着 ${n(lib.tracks)} 首歌`}>
          <p>{day(lib.empty_since)} {clock(lib.empty_since).slice(0, 5)} 起这个文件夹就读不到歌了。可能是网络共享掉线，所以先不删；连续 24 小时都是空的才自动清掉。确实不要了可以现在清。</p>
          <div className="notice-actions"><button className="btn btn-small" disabled={busy || running} onClick={forget}>现在清掉</button></div>
        </Notice>
      ) : null}
      {!linked && folder && !missing && !empty && (
        <>
          {!folder.readable && <Notice tone="error" title="程序读不了这个文件夹"><p>{folder.error}。确认挂载的目录对容器的运行身份（PUID/PGID）可读。</p></Notice>}
          {folder.readable && !folder.read_only && <Notice tone="warn" title="挂载成了可写"><p>TrackFerry 不会改动你的音乐，但建议在挂载路径末尾加上 :ro。</p></Notice>}
          <dl className="tally">
            <div><dd className="num">{n(running ? lib.seen : lib.tracks)}</dd><dt>{running ? '首已读' : '首歌'}</dt></div>
            <div><dd className="num">{n(lib.artists)}</dd><dt>位歌手</dt></div>
            {!running && last?.finished_ms ? <div><dd className="num">{clock(last.finished_ms).slice(0, 5)}</dd><dt>{day(last.finished_ms)}扫描</dt></div> : null}
            {!running && last && last.added > 0 ? <div><dd className="num">{n(last.added)}</dd><dt>首新读到</dt></div> : null}
          </dl>
          {report && report.roots.length > 0 && (
            <ul className="roots">
              {report.roots.map((r) => (
                <li key={r.name}>
                  <b>{r.name || '根目录'}</b>
                  <span className="num">{n(r.tracks)} 首</span>
                  {r.by_name > 0 && <small>{n(r.by_name)} 首没有标签，按文件名认</small>}
                  {r.unreadable > 0 && <small className="warn">{n(r.unreadable)} 个没有读取权限</small>}
                  {r.damaged > 0 && <small className="warn">{n(r.damaged)} 个内容认不出</small>}
                  {r.error && <small className="bad">{r.error}</small>}
                  {r.samples.length > 0 && <details><summary>看看是哪些</summary><ul>{r.samples.map((p) => <li key={p} className="mono">{p}</li>)}</ul></details>}
                </li>
              ))}
            </ul>
          )}
          {last?.error && <p className="field-error">{last.error}</p>}
          <div className="kept"><div><b>自动扫描</b><small>只读新增或改过的文件，没变化时几乎不费资源；有读不了的目录时，那个目录里的歌不会被删</small></div>
            <Pick label="自动扫描" value={lib.every_minutes ?? 360} choices={[15, 30, 60, 180, 360, 720, 1440].map((m) => ({ value: m, label: m < 60 ? `每 ${m} 分钟` : `每 ${m / 60} 小时` })).concat({ value: 0, label: '关' })} onPick={every} /></div>
          <dl className="kv">
            <div><dt>容器里的路径</dt><dd className="mono">{folder.path}</dd></div>
            <div><dt>认得的格式</dt><dd>MP3、FLAC、M4A、AAC、OGG、WAV</dd></div>
            <div><dt>专辑</dt><dd>按专辑名分；同名专辑看专辑艺术家，没写就看是否在同一个文件夹</dd></div>
          </dl>
        </>
      )}
      {error && <p className="field-error" role="alert">{error}</p>}
    </Setting>
  );
}
