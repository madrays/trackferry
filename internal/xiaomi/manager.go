package xiaomi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"

	"trackferry/internal/logx"
)

// renewAfter is when a serviceToken is renewed, well inside its lifetime.
const renewAfter = ServiceTokenLifetime - 2*time.Hour

// renewPause is left between renewals that fail, and between one that worked and the next
// one a refusal asks for.
const renewPause = 20 * time.Second

// DeviceSink receives the account's device list after a successful fetch.
type DeviceSink func(ctx context.Context, devices []Device) error

// QR login states reported to the UI.
const (
	QRPending    = "pending"    // shown, waiting for the scan to be confirmed in the Mi Home app
	QRConfirming = "confirming" // scan confirmed, exchanging tokens and fetching speakers
	QRLinked     = "linked"
	QRExpired    = "expired"
	QRFailed     = "failed"
)

// Sync outcomes. They exist because "login worked but no speakers appeared" has several
// different causes that other projects collapse into one empty list.
const (
	SyncOK             = "ok"
	SyncEmpty          = "empty"           // the account really has no Xiao AI speakers
	SyncRelinkRequired = "relink_required" // passToken rejected: scan again
	SyncError          = "error"           // network or upstream failure; retry later
)

// QRStatus is a snapshot of the login in progress.
type QRStatus struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	ExpiresAt time.Time `json:"expires_at"`
	Error     string    `json:"error,omitempty"`
	ErrorKind string    `json:"error_kind,omitempty"`
	Sync      *SyncInfo `json:"sync,omitempty"`
}

// SyncInfo describes the most recent device-list fetch.
type SyncInfo struct {
	At        time.Time `json:"at"`
	Outcome   string    `json:"outcome"`
	Count     int       `json:"count"`
	Variant   string    `json:"variant,omitempty"` // which list variant answered: "default" or "master"
	Error     string    `json:"error,omitempty"`
	ErrorKind string    `json:"error_kind,omitempty"`
}

// AccountStatus is what the UI shows about the linked account. It never includes tokens.
type AccountStatus struct {
	Linked         bool      `json:"linked"`
	UserID         string    `json:"user_id,omitempty"` // masked
	LinkedAt       time.Time `json:"linked_at,omitzero"`
	TokenObtained  time.Time `json:"token_obtained_at,omitzero"`
	RelinkRequired bool      `json:"relink_required"`
	LastSync       *SyncInfo `json:"last_sync,omitempty"`
}

type qrLogin struct {
	code   QRCode
	status QRStatus
	cancel context.CancelFunc
}

// Manager owns the linked account: its credentials file, QR logins, token renewal and
// device-list syncs. It is safe for concurrent use.
type Manager struct {
	client *Client
	path   string
	sink   DeviceSink
	log    *slog.Logger

	mu       sync.Mutex
	sess     *Session
	relink   bool
	lastSync *SyncInfo
	qr       *qrLogin
	pw       *pwLogin
	gen      int        // bumped by Unlink, so a login still finishing does not write back
	pwMu     sync.Mutex // serialises password login steps
	syncMu   sync.Mutex // serialises syncs and token renewals
	tried    time.Time  // last renewal that failed; with syncMu
	triedErr error
	miot     *Session  // a Mi Home token; with syncMu
	renewed  time.Time // last renewal that worked; with syncMu
}

// ErrUnlinked means the account was unlinked while the login that was linking it still ran.
var ErrUnlinked = &Error{Kind: KindExpired, Detail: "the account was unlinked meanwhile"}

// NewManager loads credentials from path (data/private/xiaomi.json) if present.
func NewManager(client *Client, path string, sink DeviceSink, log *slog.Logger) (*Manager, error) {
	m := &Manager{client: client, path: path, sink: sink, log: log}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read xiaomi credentials: %w", err)
	default:
		var s Session
		if err := json.Unmarshal(raw, &s); err != nil || s.PassToken == "" || s.UserID == "" {
			log.Warn("小米　登录文件读不出来，需要重新关联")
			m.relink = true
		} else {
			m.sess = &s
		}
	}
	return m, nil
}

func maskUserID(id string) string {
	if len(id) <= 4 {
		return "****"
	}
	return id[:2] + "****" + id[len(id)-2:]
}

// Status reports the linked account.
func (m *Manager) Status() AccountStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := AccountStatus{RelinkRequired: m.relink, LastSync: m.lastSync}
	if m.sess != nil {
		st.Linked = true
		st.UserID = maskUserID(m.sess.UserID)
		st.LinkedAt = m.sess.LinkedAt
		st.TokenObtained = m.sess.ObtainedAt
	}
	return st
}

func (m *Manager) save(s *Session) error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".xiaomi-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), m.path)
}

// Unlink forgets the account. Speakers stay in the database with their settings.
func (m *Manager) Unlink() error {
	m.CancelQR()
	m.CancelPassword()
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.mu.Lock()
	m.gen++ // a login still finishing must not write the account back
	m.sess, m.relink, m.lastSync, m.miot = nil, false, nil, nil
	m.mu.Unlock()
	if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// StartQR begins a new QR login, replacing any login still in progress.
func (m *Manager) StartQR(ctx context.Context) (QRStatus, error) {
	code, err := m.client.StartQR(ctx)
	if err != nil {
		return QRStatus{}, err
	}
	pollCtx, cancel := context.WithDeadline(context.Background(), code.Expires.Add(10*time.Second))
	login := &qrLogin{code: code, cancel: cancel, status: QRStatus{ID: NewDeviceID()[:12], State: QRPending, ExpiresAt: code.Expires}}
	m.mu.Lock()
	if m.qr != nil {
		m.qr.cancel()
	}
	m.qr = login
	st := login.status
	m.mu.Unlock()
	go m.pollLoop(pollCtx, login)
	return st, nil
}

// QR returns the current QR login, if any.
func (m *Manager) QR() (QRStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.qr == nil {
		return QRStatus{}, false
	}
	return m.qr.status, true
}

// CancelQR abandons the QR login in progress.
func (m *Manager) CancelQR() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.qr != nil {
		m.qr.cancel()
		m.qr = nil
	}
}

// QRImage prefers the official picture. The backup encodes the same pending login URL.
func (m *Manager) QRImage(ctx context.Context, id string, backup bool) ([]byte, string, error) {
	m.mu.Lock()
	login := m.qr
	m.mu.Unlock()
	if !m.qrPending(login, id) {
		return nil, "", errf(KindExpired, "no QR login is pending for this image")
	}
	var image []byte
	var ct string
	if !backup && login.code.ImageURL != "" {
		resp, body, err := m.client.get(ctx, login.code.ImageURL, nil, login.code.DeviceID)
		if err == nil && resp.StatusCode == http.StatusOK {
			image, ct = officialQR(body)
		}
	}
	if image == nil && login.code.LoginURL != "" {
		image, _ = qrcode.Encode(login.code.LoginURL, qrcode.Medium, 320)
		ct = "image/png"
	}
	if !m.qrPending(login, id) {
		return nil, "", errf(KindExpired, "no QR login is pending for this image")
	}
	if image == nil {
		return nil, "", errf(KindUpstream, "could not read or encode the Xiaomi login QR image")
	}
	return image, ct, nil
}

func (m *Manager) qrPending(login *qrLogin, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return login != nil && m.qr == login && login.status.ID == id && login.status.State == QRPending && time.Now().Before(login.status.ExpiresAt)
}

func officialQR(body []byte) ([]byte, string) {
	switch ct := http.DetectContentType(body); ct {
	case "image/png":
		if config, err := png.DecodeConfig(bytes.NewReader(body)); err == nil && config.Width <= 1024 && config.Height <= 1024 {
			if _, err := png.Decode(bytes.NewReader(body)); err == nil {
				return body, ct
			}
		}
	case "image/jpeg", "image/gif", "image/webp":
		return body, ct
	}
	return nil, ""
}

func (m *Manager) setQR(login *qrLogin, f func(*QRStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&login.status)
}

func (m *Manager) pollLoop(ctx context.Context, login *qrLogin) {
	defer login.cancel()
	failures := 0
	for {
		if ctx.Err() != nil {
			m.setQR(login, func(s *QRStatus) {
				if s.State == QRPending {
					s.State = QRExpired
				}
			})
			return
		}
		res, err := m.client.PollQR(ctx, login.code)
		if ctx.Err() != nil {
			continue
		}
		switch {
		case err != nil:
			// Upstream codes during polling usually mean the code expired; a transport
			// error might be transient, so allow a couple before giving up.
			failures++
			if failures >= 2 {
				m.setQR(login, func(s *QRStatus) {
					s.State, s.Error, s.ErrorKind = QRExpired, err.Error(), KindOf(err)
				})
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		case res.Expired:
			m.setQR(login, func(s *QRStatus) { s.State = QRExpired })
			return
		case res.Done:
			m.setQR(login, func(s *QRStatus) { s.State = QRConfirming })
			m.finishLogin(login, res)
			return
		default:
			failures = 0
		}
	}
}

func (m *Manager) finishLogin(login *qrLogin, res PollResult) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := m.link(ctx, Session{UserID: res.UserID, CUserID: res.CUserID, PassToken: res.PassToken, DeviceID: login.code.DeviceID})
	if err != nil {
		m.setQR(login, func(st *QRStatus) {
			st.State, st.Error, st.ErrorKind = QRFailed, "扫码成功，但换取音箱服务令牌失败："+err.Error(), KindOf(err)
		})
		return
	}
	m.setQR(login, func(st *QRStatus) { st.State, st.Sync = QRLinked, &info })
}

// link turns a fresh passToken into a stored, linked account and runs the first sync.
func (m *Manager) link(ctx context.Context, s Session) (SyncInfo, error) {
	m.mu.Lock()
	gen := m.gen
	m.mu.Unlock()
	s.LinkedAt = time.Now()
	s, err := m.client.Exchange(ctx, s)
	// The account may have been unlinked while the exchange ran: a login that finishes after
	// an Unlink is not to bring the account or its credentials file back.
	if err == nil && m.genOf() != gen {
		err = ErrUnlinked
	}
	if err == nil {
		err = m.save(&s)
	}
	if err != nil {
		m.log.Warn("小米　关联失败", "类型", KindOf(err), "原因", err)
		return SyncInfo{}, err
	}
	m.mu.Lock()
	if m.gen != gen {
		m.mu.Unlock()
		return SyncInfo{}, ErrUnlinked
	}
	m.sess, m.relink = &s, false
	m.mu.Unlock()
	m.syncMu.Lock()
	m.triedErr = nil
	m.syncMu.Unlock()
	m.log.Info("小米　已关联", "账号", maskUserID(s.UserID))
	return m.Sync(ctx), nil
}

// genOf is the login generation: Unlink bumps it.
func (m *Manager) genOf() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

func (m *Manager) session() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sess == nil {
		return nil
	}
	c := *m.sess
	return &c
}

// renew exchanges the passToken for a fresh serviceToken. Callers hold syncMu.
func (m *Manager) renew(ctx context.Context, s *Session) (*Session, error) {
	// Every speaker asks at once when the token lapses; the account service is asked once.
	if m.triedErr != nil && time.Since(m.tried) < renewPause {
		return nil, m.triedErr
	}
	ns, err := m.client.Exchange(ctx, *s)
	if err != nil {
		if ctx.Err() == nil {
			m.tried, m.triedErr = time.Now(), err
		}
		if KindOf(err) == KindAuth {
			m.mu.Lock()
			if m.sess != nil && m.sess.PassToken == s.PassToken {
				m.relink = true
			}
			m.mu.Unlock()
		}
		return nil, err
	}
	m.triedErr, m.renewed = nil, time.Now()
	// A full disk must not stop the speakers: the token works until the next start.
	if err := m.save(&ns); err != nil {
		m.log.Warn("小米　新的登录没有存上", "原因", err)
	}
	m.mu.Lock()
	if m.sess != nil && m.sess.UserID == ns.UserID {
		m.sess, m.relink = &ns, false
	}
	m.mu.Unlock()
	m.log.Info("小米　登录已续期")
	return &ns, nil
}

// Sync fetches the device list and hands it to the sink. Both list variants are tried
// before concluding the account is empty, and every failure is reported with its cause.
func (m *Manager) Sync(ctx context.Context) SyncInfo {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	info := m.sync(ctx)
	m.mu.Lock()
	m.lastSync = &info
	m.mu.Unlock()
	m.log.Info("小米　读取音箱", "结果", info.Outcome, "台数", info.Count, "类型", info.ErrorKind)
	return info
}

func (m *Manager) sync(ctx context.Context) SyncInfo {
	info := SyncInfo{At: time.Now()}
	fail := func(outcome string, err error) SyncInfo {
		info.Outcome, info.Error, info.ErrorKind = outcome, err.Error(), KindOf(err)
		return info
	}
	s := m.session()
	if s == nil {
		return fail(SyncRelinkRequired, errf(KindAuth, "no Xiaomi account is linked"))
	}
	m.mu.Lock()
	relink := m.relink
	m.mu.Unlock()
	if time.Since(s.ObtainedAt) > renewAfter || relink {
		ns, err := m.renew(ctx, s)
		if err != nil {
			if KindOf(err) == KindAuth {
				return fail(SyncRelinkRequired, err)
			}
			return fail(SyncError, err)
		}
		s = ns
	}
	var firstErr error
	renewed := false
	for _, v := range []struct {
		name   string
		master bool
	}{{"default", false}, {"master", true}} {
		devs, err := m.client.DeviceList(ctx, *s, v.master)
		if errors.Is(err, ErrUnauthorized) && !renewed {
			renewed = true
			ns, rerr := m.renew(ctx, s)
			if rerr != nil {
				if KindOf(rerr) == KindAuth {
					return fail(SyncRelinkRequired, rerr)
				}
				return fail(SyncError, rerr)
			}
			s = ns
			devs, err = m.client.DeviceList(ctx, *s, v.master)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(devs) == 0 {
			continue
		}
		if err := m.sink(ctx, devs); err != nil {
			return fail(SyncError, fmt.Errorf("save devices: %w", err))
		}
		info.Outcome, info.Count, info.Variant = SyncOK, len(devs), v.name
		return info
	}
	if firstErr != nil {
		if errors.Is(firstErr, ErrUnauthorized) {
			return fail(SyncRelinkRequired, firstErr)
		}
		return fail(SyncError, firstErr)
	}
	info.Outcome = SyncEmpty
	return info
}

// Run renews the serviceToken before it lapses and re-syncs devices periodically.
func (m *Manager) Run(ctx context.Context) {
	if m.session() != nil {
		m.guarded(func() { m.Sync(ctx) })
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for i := 1; ; i++ {
		// A sync that failed (the NAS starts before its network does) is not left for an hour.
		var soon <-chan time.Time
		if st := m.Status(); st.Linked && st.LastSync != nil && st.LastSync.Outcome == SyncError {
			soon = time.After(time.Minute)
		}
		late := false
		select {
		case <-ctx.Done():
			return
		case <-soon:
			late = true
		case <-t.C:
		}
		s := m.session()
		if s == nil {
			continue
		}
		// A login taken for expired is tried again now and then: it may work after all.
		if late || m.Status().RelinkRequired || time.Since(s.ObtainedAt) > renewAfter || i%6 == 0 {
			m.guarded(func() { m.Sync(ctx) }) // Sync renews the token when it is close to expiry
		}
	}
}

// guarded keeps a fault in one round from ending the process.
func (m *Manager) guarded(f func()) {
	defer logx.Recover(m.log, "小米")
	f()
}

// ErrNotLinked means there is no usable Xiaomi login.
var ErrNotLinked = &Error{Kind: KindAuth, Detail: "no Xiaomi account is linked"}

// Conversations reads one speaker's newest records, renewing the serviceToken once when
// Mina rejects it.
func (m *Manager) Conversations(ctx context.Context, deviceID, hardware string, limit int) ([]Record, error) {
	var recs []Record
	err := m.withSession(ctx, func(s Session) (err error) {
		recs, err = m.client.Conversations(ctx, s, deviceID, hardware, limit)
		return err
	})
	return recs, err
}
