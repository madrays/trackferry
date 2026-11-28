import { useCallback, useEffect, useState } from 'react';
import { api, ApiError, setUnauthorizedHandler, type Account as AccountT, type Device, type Session, type SyncInfo } from './api';
import { href, morph, readPlace, type Page, type Place } from './nav';
import { Stage } from './stage/Stage';
import { Login, Setup } from './pages/Auth';
import { Home } from './pages/Home';
import { Brand } from './components/Mark';
import { DeskBar, Dock } from './components/Dock';
import { Fish } from './components/Fish';
import { HereSaid } from './components/HereSaid';
import { Signature } from './components/Signature';
import { lookAt } from './ear';
import { TVPresence } from './components/TVPresence';
import { monitorTV, tvTarget } from './tv';

// Pages arrive on their own, right after the first one is up.
const load = {
  tv: () => import('./components/TVDevices'), welcome: () => import('./pages/Onboarding'), flow: () => import('./pages/FlowPage'), say: () => import('./pages/Say'),
  search: () => import('./pages/Search'), more: () => import('./pages/More'), library: () => import('./pages/Library'), speakers: () => import('./pages/Speakers'), account: () => import('./pages/Account'),
};
type Loaded = { [K in keyof typeof load]?: Awaited<ReturnType<(typeof load)[K]>> };
const got: Loaded = {};
const reloaded = () => { try { return Number(sessionStorage.getItem('trackferry.reloaded')); } catch { return Date.now(); } };
const ready = async (p: Page) => {
  if (p === 'home' || got[p]) return;
  try { (got as Record<string, unknown>)[p] = await load[p](); } catch {
    // A newer build replaced the files; once, so that being offline does not loop.
    if (Date.now() - reloaded() > 30000) { try { sessionStorage.setItem('trackferry.reloaded', String(Date.now())); } catch { return; } location.reload(); }
  }
};

const tabs: { page: Page; label: string }[] = [
  { page: 'home', label: '此刻' },
  { page: 'search', label: '搜索' },
  { page: 'library', label: '曲库' },
  { page: 'flow', label: '流转' },
  { page: 'say', label: '怎么说' },
  { page: 'speakers', label: '音箱' },
  { page: 'tv', label: '电视' },
  { page: 'account', label: '设置' },
];

interface Data {
  account: AccountT;
  devices: Device[];
  takeoverSupported: boolean;
}

export default function App() {
  return <Stage><Inner /></Stage>;
}

function Inner() {
  const [session, setSession] = useState<Session | null>(null);
  const [fatal, setFatal] = useState<string | null>(null);
  const [data, setData] = useState<Data | null>(null);
  const [place, setPlace] = useState<Place | null>(readPlace());
  const [, arrived] = useState(0);
  const [pairing, setPairing] = useState(false);
  useEffect(() => { if (place?.page === 'home' && place.id) lookAt(tvTarget(place.id)); }, [place]);
  useEffect(() => { if (place) ready(place.page).then(() => arrived((n) => n + 1)); }, [place?.page]); // eslint-disable-line react-hooks/exhaustive-deps
  const [scanOnAccount, setScanOnAccount] = useState(false);

  const loadSession = useCallback(async () => {
    try {
      setFatal(null);
      setSession(await api.session());
    } catch (e) {
      setFatal((e as ApiError).message);
    }
  }, []);

  const loadData = useCallback(async () => {
    const [x, d] = await Promise.all([api.xiaomi(), api.devices()]);
    setData({ account: x.account, devices: d.devices, takeoverSupported: d.takeover_supported });
    return { account: x.account, devices: d.devices };
  }, []);

  useEffect(() => {
    setUnauthorizedHandler(() => setSession((s) => (s ? { ...s, authenticated: false } : s)));
    loadSession();
    const onPop = () => { const p = readPlace(); ready(p?.page ?? 'home').then(() => morph(() => setPlace(p))); };
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, [loadSession]);

  useEffect(() => {
    if (!session?.authenticated) return;
    loadData()
      .then(({ account, devices }) => {
        if (readPlace()) return;
        // Fresh install: walk through the setup. Otherwise land on the stage.
        let skipped = false;
        try { skipped = localStorage.getItem('trackferry.skip-xiaomi') === '1'; } catch { /* no storage: ask again */ }
        const p: Place = { page: !account.linked && devices.length === 0 && !skipped ? 'welcome' : 'home' };
        history.replaceState(null, '', href(p));
        setPlace(p);
      })
      .catch((e) => setFatal((e as ApiError).message));
  }, [session?.authenticated, loadData]);

  const navigate = useCallback((p: Place, from?: Element | null, scan = false) => {
    const go = () => ready(p.page).then(() => morph(() => {
      setScanOnAccount(scan);
      setPlace(p);
      scrollTo(0, 0);
    }, from));
    if (location.hash !== href(p)) history.pushState(null, '', href(p));
    go();
  }, []);
  useEffect(() => {
    if (!data) return;
    const t = setTimeout(() => (Object.keys(load) as (keyof typeof load)[]).forEach((p) => load[p]().then((m) => { (got as Record<string, unknown>)[p] = m; }, () => {})), 1200);
    return () => clearTimeout(t);
  }, [!data]); // eslint-disable-line react-hooks/exhaustive-deps
  const relink = useCallback(() => navigate({ page: 'account' }, null, true), [navigate]);
  const refresh = useCallback(async () => { await loadData(); }, [loadData]);

  useEffect(() => { if (session?.authenticated) return monitorTV(); }, [session?.authenticated]);

  const [scrolled, setScrolled] = useState(false);
  useEffect(() => {
    const f = () => setScrolled(scrollY > 24);
    addEventListener('scroll', f, { passive: true });
    return () => removeEventListener('scroll', f);
  }, []);

  if (fatal) {
    return (
      <main className="auth">
        <div className="auth-card">
          <h1>打不开管理页</h1>
          <p className="lead">{fatal}</p>
          <button className="btn btn-primary btn-block" onClick={() => { setFatal(null); loadSession(); }}>重试</button>
        </div>
      </main>
    );
  }
  if (!session) return <div className="boot"><Fish mood="searching" /></div>;
  if (session.setup_required) return <Setup onDone={loadSession} />;
  if (!session.authenticated) return <Login onDone={loadSession} />;
  if (!data || !place) return <div className="boot"><Fish mood="searching" /></div>;

  const logout = async () => {
    await api.logout().catch(() => {});
    setData(null);
    setSession({ ...session, authenticated: false });
  };
  const onDevices = (devices: Device[], sync?: SyncInfo) =>
    setData((d) => (d ? { ...d, devices, account: sync ? { ...d.account, last_sync: sync, relink_required: sync.outcome === 'relink_required' } : d.account } : d));
  const needsAttention = data.account.relink_required || !data.account.linked;

  return (
    <div className={`shell at-${place.page}`}>
      <header className={`topbar ${scrolled ? 'solid' : ''}`}>
        <a className="brand" href="#/home" aria-label="TrackFerry" onClick={(e) => { e.preventDefault(); navigate({ page: 'home' }); }}><Brand /></a>
        <TVPresence place={place} navigate={navigate} pairing={pairing} setPairing={setPairing} />
        {place.page !== 'welcome' && (
          <>
          {(['flow', 'say', 'speakers', 'account', 'tv'] as Page[]).includes(place.page) && <button type="button" className="crumb glass" onClick={() => navigate({ page: 'more' })}><svg viewBox="0 0 10 16" width="10" height="16" aria-hidden="true"><path d="M8.5 1.5 2 8l6.5 6.5" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" /></svg>更多</button>}
          <nav className="tabs" aria-label="主导航">
            {tabs.map((t) => (
              <a key={t.page} href={href(t)} aria-current={place.page === t.page ? 'page' : undefined} onClick={(e) => { e.preventDefault(); navigate({ page: t.page }); }}>
                {t.label}{t.page === 'account' && needsAttention && <span className="dot" aria-label="需要处理" />}
              </a>
            ))}
          </nav>
          </>
        )}
      </header>
      <main className="content">
        {place.page === 'welcome' && got.welcome && (
          <got.welcome.Onboarding
            initialStep={data.account.linked && !data.account.relink_required ? 1 : 0}
            onFinish={async () => { await refresh(); navigate({ page: 'home' }); }}
          />
        )}
        {place.page === 'home' && <Home devices={data.devices} navigate={navigate} onRelink={relink} />}
        {place.page === 'search' && got.search && <got.search.Search devices={data.devices} navigate={navigate} />}
        {place.page === 'more' && got.more && <got.more.More navigate={navigate} dot={needsAttention} />}
        {place.page === 'flow' && got.flow && <got.flow.FlowPage key={place.id ?? ''} id={place.id} devices={data.devices} />}
        {place.page === 'say' && got.say && <got.say.Say navigate={navigate} />}
        {place.page === 'library' && got.library && <got.library.Library place={place} devices={data.devices} navigate={navigate} />}
        {place.page === 'speakers' && got.speakers && <got.speakers.Speakers account={data.account} devices={data.devices} takeoverSupported={data.takeoverSupported} onDevices={onDevices} onRescan={relink} />}
        {place.page === 'tv' && got.tv && <got.tv.TVDevices onPair={() => setPairing(true)} />}
        {place.page === 'account' && got.account && (
          <got.account.Account key={String(scanOnAccount)} account={data.account} build={session.build} startScanning={scanOnAccount} onChanged={refresh} onLogout={logout} />
        )}
      </main>
      <HereSaid />
      {place.page !== 'welcome' && <Dock page={place.page} devices={data.devices} navigate={navigate} dot={needsAttention} />}
      <DeskBar page={place.page} devices={data.devices} navigate={navigate} />
      {place.page !== 'account' && place.page !== 'search' && place.page !== 'more' && <footer className="colophon"><Signature /></footer>}
    </div>
  );
}
