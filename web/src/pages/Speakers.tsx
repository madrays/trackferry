import { useEffect, useState } from 'react';
import { api, ApiError, type Account, type Device, type ObserverStatus, type SyncInfo } from '../api';
import { Notice } from '../components/Notice';
import { SpeakerRow } from '../components/Speaker';
import { useBackdrop } from '../stage/Stage';
import { explainSync, timeAgo } from '../copy';

export function Speakers({ account, devices, onDevices, onRescan }: {
  account: Account;
  devices: Device[];
  takeoverSupported: boolean;
  onDevices: (d: Device[], sync?: SyncInfo) => void;
  onRescan: () => void;
}) {
  useBackdrop();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [justSynced, setJustSynced] = useState(false);
  const [obs, setObs] = useState<ObserverStatus | null>(null);

  useEffect(() => {
    const load = () => api.observer().then(setObs).catch(() => {});
    load();
    const t = setInterval(() => { if (!document.hidden) load(); }, 4000);
    return () => clearInterval(t);
  }, []);

  const sync = async () => {
    setBusy(true);
    setError(null);
    try {
      const r = await api.sync();
      onDevices(r.devices, r.sync);
      setJustSynced(true);
    } catch (e) {
      setError((e as ApiError).message);
    } finally {
      setBusy(false);
    }
  };

  const update = (d: Device) => onDevices(devices.map((x) => (x.device_id === d.device_id ? d : x)));
  const present = devices.filter((d) => d.in_account);
  const gone = devices.filter((d) => !d.in_account);
  const last = account.last_sync;
  const explanation = last ? explainSync(last) : null;
  const watchOf = (d: Device) => obs?.devices.find((w) => w.device_id === d.device_id);
  const row = (d: Device) => <SpeakerRow key={d.device_id} device={d} watch={watchOf(d)} onChange={update} tests={watchOf(d)?.state !== 'unsupported'} />;

  return (
    <section className="speakers">
      <header className="page-head">
        <h1>音箱</h1>
        <div className="page-head-side">
          {last && <small className="muted">{timeAgo(last.at)}同步</small>}
          <button className="btn btn-small" onClick={sync} disabled={busy || !account.linked}>
            {busy ? <><span className="spinner spinner-tiny" />正在同步…</> : '从小米同步'}
          </button>
        </div>
      </header>

      {!account.linked && !account.relink_required && present.length > 0 && (
        <Notice tone="warn" title="还没有关联小米账号">
          <p>关联之前列表不会更新，也收不到点歌。</p>
          <div className="notice-actions"><button className="btn btn-small" onClick={onRescan}>关联账号</button></div>
        </Notice>
      )}
      {account.relink_required && (
        <Notice tone="error" title="小米登录已失效">
          <p>重新登录后自动恢复，音箱设置会保留。</p>
          <div className="notice-actions"><button className="btn btn-small" onClick={onRescan}>重新登录</button></div>
        </Notice>
      )}
      {!account.relink_required && explanation && (explanation.tone !== 'ok' || justSynced) && (
        <Notice tone={explanation.tone} title={explanation.title}>
          {explanation.body && <p>{explanation.body}</p>}
          {explanation.action === 'rescan' && <div className="notice-actions"><button className="btn btn-small" onClick={onRescan}>换个账号登录</button></div>}
        </Notice>
      )}
      {error && <Notice tone="error" title={error} />}
      {obs?.paused === 'off' && <Notice tone="info" title="听歌识别已关闭"><p>说话点歌没反应；从网页、电视、Home Assistant 送歌照常。</p><div className="notice-actions"><a className="btn btn-small" href="#/account">去打开</a></div></Notice>}
      {obs?.paused === 'sleep' && <Notice tone="info" title="在睡眠时段"><p>这段时间不读对话记录，说话点歌没反应；从网页送歌照常。</p></Notice>}

      {present.length > 0 ? (
        <ul className="spk-list">{present.map(row)}</ul>
      ) : (
        <div className="empty">
          <p>{account.linked ? '这个小米账号里没有音箱。' : '还没有关联小米账号。'}</p>
          <button className="btn btn-primary" onClick={onRescan}>{account.linked ? '换个账号登录' : '关联小米账号'}</button>
        </div>
      )}
      {gone.length > 0 && <><h2 className="sub">已不在账号中</h2><ul className="spk-list">{gone.map(row)}</ul></>}
      {account.linked && <p className="spk-foot muted">多久读一次音箱、没人说话时放慢、夜里睡眠时段，在<a href="#/account">设置 → 听歌识别</a>。常被小米限流时调长间隔。</p>}

    </section>
  );
}
