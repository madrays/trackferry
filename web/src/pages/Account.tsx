import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { api, ApiError, type Account as AccountT, type LinkResult, type UpdateStatus } from '../api';
import { Notice } from '../components/Notice';
import { FeiniuCard } from '../components/Feiniu';
import { FolderCard } from '../components/Folder';
import { HomeAssistant } from '../components/HomeAssistant';
import { LockIcon, Setting, type Light } from '../components/Setting';
import { SourceIcon } from '../components/SourceIcon';
import { useInstall } from '../pwa';
import { Signature } from '../components/Signature';
import { Playing } from '../components/Playing';
import { Listening } from '../components/Listening';
import { ThisDevice } from '../components/ThisDevice';
import { Sources } from '../components/Sources';
import { useBackdrop } from '../stage/Stage';
import { LinkAccount } from '../components/LinkAccount';
import { explainSync, timeAgo } from '../copy';
import fish from '../assets/brand/fish-mark.webp';
import word from '../assets/brand/wordmark.svg';
import './settings.css';

const parts = [['feiniu', '飞牛音乐'], ['folder', '音乐文件夹'], ['xiaomi', '小米账号'], ['listen', '听歌识别'], ['sources', '线上音源'], ['playing', '连续播放'], ['home', 'Home Assistant'], ['password', '管理密码'], ['about', '关于']] as const;
type Part = (typeof parts)[number][0];

export function Account({ account, build, startScanning, onChanged, onLogout }: {
  account: AccountT;
  build: { version: string; commit: string };
  startScanning: boolean;
  onChanged: () => Promise<void>;
  onLogout: () => void;
}) {
  useBackdrop();
  const [scanning, setScanning] = useState(startScanning);
  const [linkedNow, setLinkedNow] = useState<LinkResult | null>(null);
  const [confirmUnlink, setConfirmUnlink] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const onLinked = async (qr: LinkResult) => {
    setLinkedNow(qr);
    setScanning(false);
    await onChanged();
  };

  const unlink = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.unlink();
      setConfirmUnlink(false);
      setLinkedNow(null);
      await onChanged();
    } catch (e) {
      setError((e as ApiError).message);
    } finally {
      setBusy(false);
    }
  };

  const [lights, setLights] = useState<Partial<Record<Part, Light>>>({});
  const feiniuLight = useCallback((l: Light) => setLights((x) => ({ ...x, feiniu: l })), []);
  const folderLight = useCallback((l: Light) => setLights((x) => ({ ...x, folder: l === 'off' ? undefined : l })), []);
  const sourceLight = useCallback((l: Light) => setLights((x) => ({ ...x, sources: l })), []);
  const homeLight = useCallback((l: Light) => setLights((x) => ({ ...x, home: l })), []);
  const aboutLight = useCallback((l: Light | null) => setLights((x) => ({ ...x, about: l ?? undefined })), []);
  const mine: Light = !account.linked ? 'warn' : account.relink_required ? 'bad' : 'ok';
  const [at, setAt] = useState<Part>('feiniu');
  useEffect(() => {
    const f = () => {
      const tops = parts.map(([p]) => [p, document.getElementById(`set-${p}`)?.getBoundingClientRect().top ?? 1e6] as const);
      const end = innerHeight + scrollY >= document.documentElement.scrollHeight - 4 && scrollY > 0;
      setAt(scrollY < 8 ? 'feiniu' : end ? 'about' : (tops.filter(([, y]) => y < innerHeight * .35).at(-1)?.[0] ?? 'feiniu'));
    };
    f();
    addEventListener('scroll', f, { passive: true });
    return () => removeEventListener('scroll', f);
  }, []);
  const go = (p: Part) => document.getElementById(`set-${p}`)?.scrollIntoView({ behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth', block: 'start' });

  const explanation = linkedNow?.sync ? explainSync(linkedNow.sync) : null;

  return (
    <div className="settings">
      <header className="page-head"><h1>设置</h1></header>
      <div className="st-cols">
      <nav className="st-nav" aria-label="设置分类">
        {parts.map(([p, name]) => { const l = p === 'xiaomi' ? mine : lights[p]; return <button key={p} type="button" aria-current={at === p || undefined} onClick={() => go(p)}>{name}{l && <i className={l} />}</button>; })}
      </nav>
      <div className="st-list">
      {explanation && <Notice tone={explanation.tone} title={`已关联新的登录，${explanation.title}`}>{explanation.body && <p>{explanation.body}</p>}</Notice>}

      <FeiniuCard onLight={feiniuLight} />
      <FolderCard onLight={folderLight} />

      <Setting id="xiaomi" force={scanning} icon={<SourceIcon name="xiaomi" />} title="小米账号" light={mine} state={account.linked ? (account.relink_required ? '登录已失效' : '已关联') : '未关联'}
        says={account.last_sync && account.linked ? explainSync(account.last_sync).title : '用来读取音箱听到的话和控制播放'}
        actions={scanning ? undefined : <><button className={`btn ${account.linked && !account.relink_required ? '' : 'btn-primary'}`} onClick={() => setScanning(true)}>{account.linked ? '重新登录' : '关联小米账号'}</button>{account.linked && !confirmUnlink && <button className="btn btn-quiet" onClick={() => setConfirmUnlink(true)}>解除关联</button>}</>}>
          {account.linked && <dl className="kv">
            {account.user_id && <div><dt>小米 ID</dt><dd className="mono">{account.user_id}</dd></div>}
            {account.linked_at && <div><dt>关联</dt><dd>{timeAgo(account.linked_at)}</dd></div>}
            {account.token_obtained_at && <div><dt>访问令牌</dt><dd>{timeAgo(account.token_obtained_at)}更新，到期前自动续期</dd></div>}
            {account.last_sync && <div><dt>读取音箱</dt><dd>{timeAgo(account.last_sync.at)}</dd></div>}
          </dl>}
          {confirmUnlink && (
            <div className="confirm">
              <p>解除后会删除 NAS 上的小米登录凭据，不再读取口令。音箱设置会保留，下次关联后恢复。</p>
              <div className="card-actions">
                <button className="btn btn-danger" onClick={unlink} disabled={busy}>{busy ? '正在解除…' : '确认解除'}</button>
                <button className="btn btn-quiet" onClick={() => setConfirmUnlink(false)}>取消</button>
              </div>
            </div>
          )}
          {error && <p className="field-error" role="alert">{error}</p>}
          {scanning && (
            <div className="linking">
              {account.linked && <p className="hint">登录成功后替换现在的登录。<button className="link" onClick={() => { api.cancelQR().catch(() => {}); api.cancelPassword().catch(() => {}); setScanning(false); }}>取消</button></p>}
              <LinkAccount onLinked={onLinked} />
            </div>
          )}
      </Setting>

      <Listening />
      <Sources onLight={sourceLight} />
      <ThisDevice />
      <Playing />
      <HomeAssistant onLight={homeLight} />
      <PasswordCard onLogout={onLogout} />
      <About build={build} onLight={aboutLight} />
      </div>
      </div>
    </div>
  );
}

function PasswordCard({ onLogout }: { onLogout: () => void }) {
  const [open, setOpen] = useState(false);
  const [cur, setCur] = useState('');
  const [next, setNext] = useState('');
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setMsg(null);
    try {
      await api.changePassword(cur, next);
      setMsg({ ok: true, text: '已修改。其他浏览器已退出登录。' });
      setCur('');
      setNext('');
      setOpen(false);
    } catch (err) {
      setMsg({ ok: false, text: (err as ApiError).message });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Setting id="password" icon={<LockIcon />} title="管理密码" says="修改后其他浏览器会退出登录"
      actions={<>{!open && <button className="btn" onClick={() => setOpen(true)}>修改密码</button>}<button className="btn btn-quiet" onClick={onLogout}>退出登录</button></>}>
      {msg && <p className={msg.ok ? 'ok-text' : 'field-error'}>{msg.text}</p>}
      {open && (
        <form className="form form-inline" onSubmit={submit}>
          <label className="field"><span>当前密码</span><input type="password" value={cur} onChange={(e) => setCur(e.target.value)} autoComplete="current-password" required /></label>
          <label className="field"><span>新密码（至少 8 位）</span><input type="password" value={next} onChange={(e) => setNext(e.target.value)} autoComplete="new-password" minLength={8} required /></label>
          <div className="card-actions">
            <button className="btn btn-primary" disabled={busy}>{busy ? '正在保存…' : '保存'}</button>
            <button type="button" className="btn btn-quiet" onClick={() => setOpen(false)}>取消</button>
          </div>
        </form>
      )}
    </Setting>
  );
}

function About({ build, onLight }: { build: { version: string; commit: string }; onLight: (l: Light | null) => void }) {
  const install = useInstall();
  const [up, setUp] = useState<UpdateStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => { api.update().then(setUp).catch(() => {}); }, []);
  useEffect(() => { onLight(up?.newer ? 'warn' : null); }, [up?.newer, onLight]);
  const check = async () => {
    setBusy(true);
    setError(null);
    try { setUp(await api.checkUpdate()); } catch (e) { setError((e as ApiError).message); } finally { setBusy(false); }
  };
  return (
    <section className="st" id="set-about">
      <div className="st-about">
        <img src={fish} alt="" width={96} height={96} />
        <div>
          <img src={word} alt="TrackFerry" width={150} />
          <p>小爱点歌，曲库优先</p>
          <small className="num">版本 {build.version}{build.commit && build.commit !== 'unknown' && `（${build.commit}）`}{up && !up.newer && up.checked_ms ? up.error ? `，${up.error}` : '，已是最新' : ''}</small>
          {up?.failed && !up.newer && <small className="field-error">{up.failed} 没能启动，已退回</small>}
        </div>
        {install && <button className="btn" onClick={install}>装到桌面</button>}
        <button className="btn" onClick={check} disabled={busy}>{busy ? <><span className="spinner spinner-tiny" />正在检查</> : '检查更新'}</button>
      </div>
      {error && <p className="field-error st-body">{error}</p>}
      {up && <UpdateProxy saved={up.proxy ?? ''} onStatus={setUp} />}
      {up?.newer && (
        <div className="st-body st-update">
          <h3><b className="num">{up.latest}</b>{up.published && <small>{new Date(up.published).toLocaleDateString('zh-CN')}发布</small>}{up.url && <a href={up.url} target="_blank" rel="noreferrer">更新说明</a>}</h3>
          {up.notes && <pre className="st-notes">{up.notes.replace(/^#+ +/gm, '').replace(/^[-*] +/gm, '· ')}</pre>}
          <Install up={up} running={build.version} onStatus={setUp} />
          {up.image === 'ffmpeg'
            ? <div><p>你用的是带 ffmpeg 的镜像，不提供页面内更新，只能拉新镜像：在 compose.yaml 所在目录执行下面的命令，数据不受影响。镜像写了固定版本号的，先改成 ffmpeg 或 <span className="num">{up.latest}-ffmpeg</span>：</p><pre>docker compose pull &amp;&amp; docker compose up -d</pre></div>
            : <div><p>{up.installable && '也可以用命令更新：'}在 compose.yaml 所在目录执行下面的命令，数据不受影响。镜像写了固定版本号的，先改成 latest 或 <span className="num">{up.latest}</span>：</p><pre>docker compose pull &amp;&amp; docker compose up -d</pre></div>}
        </div>
      )}
      <div className="st-sig"><Signature /></div>
    </section>
  );
}

/** Installs the new version and waits until it answers. */
function Install({ up, running, onStatus }: { up: UpdateStatus; running: string; onStatus: (u: UpdateStatus) => void }) {
  const [asking, setAsking] = useState(false);
  const [waiting, setWaiting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const on = up.install && !up.install.error ? up.install : null;
  const active = waiting || !!on;
  useEffect(() => {
    if (!active) return;
    let gone = false, down = false;
    const began = Date.now();
    const tick = async () => {
      try {
        const s = await api.session();
        if (gone) return;
        if (s.build.version !== running) return location.reload();
        const u = await api.update();
        if (gone) return;
        if (u.install?.error) { setWaiting(false); onStatus(u); return; }
        if (!u.install) { // it restarted, and this is still the old version
          if (u.failed) { setWaiting(false); setError(`${u.failed} 启动不了，已退回 ${running}，数据也已还原`); onStatus({ ...up, install: undefined, failed: u.failed }); return; }
        } else if (!down) onStatus(u);
      } catch { down = true; }
      if (gone) return;
      if (Date.now() - began > 35 * 60e3) { setWaiting(false); setError('等了很久没有结果，请看容器日志'); return; }
      timer = setTimeout(tick, 1000);
    };
    let timer = setTimeout(tick, 600);
    return () => { gone = true; clearTimeout(timer); };
  }, [active]);

  if (!up.installable) return null;
  const start = async () => {
    setAsking(false);
    setError(null);
    try { onStatus(await api.installUpdate()); setWaiting(true); } catch (e) { setError((e as ApiError).message); }
  };
  const percent = on?.total && on.total > 0 && on.done ? Math.min(99, Math.round(on.done / on.total * 100)) : 0;
  const says = !on || on.phase === 'restart' ? '正在重启' : on.phase === 'check' ? '正在校验' : `正在下载${percent ? ` ${percent}%` : ''}`;
  const problem = error ?? up.install?.error ?? (up.failed === up.latest ? `${up.failed} 上次没能启动，已退回 ${running}` : null);
  return (
    <div>
      {active ? <p className="st-progress" role="status"><span className="spinner spinner-tiny" /><span className="num">{says}</span></p>
        : asking ? <div className="confirm">
          <p>会先备份数据库，再换上 {up.latest} 并重启，正在放的歌会停。新版本起不来时自动退回。</p>
          <div className="card-actions"><button className="btn btn-primary" onClick={start}>确认更新</button><button className="btn btn-quiet" onClick={() => setAsking(false)}>取消</button></div>
        </div>
        : <button className="btn btn-primary" onClick={() => setAsking(true)}>更新到 <span className="num">{up.latest}</span></button>}
      {problem && !active && <p className="field-error" role="alert">{problem}</p>}
    </div>
  );
}

/** Where update checks and downloads go through; nothing else uses it. */
function UpdateProxy({ saved, onStatus }: { saved: string; onStatus: (u: UpdateStatus) => void }) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState(saved);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const save = async (next: string) => {
    setBusy(true);
    setMsg(null);
    try {
      const u = await api.updateProxy(next.trim());
      onStatus(u);
      setOpen(false);
      setMsg(u.error ? { ok: false, text: `已保存，但${u.error}` } : { ok: true, text: next.trim() ? '已保存，经代理连得上 GitHub' : '已改为直接连接' });
    } catch (e) {
      setMsg({ ok: false, text: (e as ApiError).message });
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="st-body st-proxy">
      {!open && <p className="hint">{saved ? <>更新代理 <span className="num">{saved}</span></> : '连不上 GitHub 时可以设置代理，只用于检查和下载更新'}<button className="link" onClick={() => { setValue(saved); setMsg(null); setOpen(true); }}>{saved ? '修改' : '设置代理'}</button></p>}
      {open && (
        <form className="form form-inline" onSubmit={(e) => { e.preventDefault(); void save(value); }}>
          <label className="field"><span>更新代理（http、https 或 socks5）</span><input type="text" value={value} onChange={(e) => setValue(e.target.value)} placeholder="http://192.168.1.2:7890" autoComplete="off" spellCheck={false} autoCapitalize="none" /></label>
          <div className="card-actions">
            <button className="btn btn-primary" disabled={busy}>{busy ? '正在测试…' : '保存并测试'}</button>
            {saved && <button type="button" className="btn btn-quiet" disabled={busy} onClick={() => void save('')}>不用代理</button>}
            <button type="button" className="btn btn-quiet" disabled={busy} onClick={() => setOpen(false)}>取消</button>
          </div>
        </form>
      )}
      {msg && <p className={msg.ok ? 'ok-text' : 'field-error'} role="status">{msg.text}</p>}
    </div>
  );
}
