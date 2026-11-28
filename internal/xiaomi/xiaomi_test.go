package xiaomi

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skip2/go-qrcode"
)

// fake imitates the account service and Mina API closely enough to drive the whole flow.
type fake struct {
	srv          *httptest.Server
	scanned      chan struct{} // closed when the "user" confirms the scan
	passValid    atomic.Bool
	tokenSerial  atomic.Int32
	acceptToken  atomic.Value // string: serviceToken Mina currently accepts
	plainEmpty   bool         // plain device_list returns [] and only master=1 has devices
	listCalls    sync.Map
	exchangeHits atomic.Int32
	holdExchange chan struct{} // when armed, the exchange waits, for racing a login against Unlink
	pwFlow       string        // password flow: "direct", "captcha" or "verify"
	pwSubmits    atomic.Int32
	ubusMu       sync.Mutex
	ubusCalls    []url.Values // forms received by /remote/ubus
}

const nonce = "1234567890123456789012" // beyond float64 precision on purpose

func newFake(t *testing.T) *fake {
	f := &fake{scanned: make(chan struct{})}
	f.passValid.Store(true)
	f.acceptToken.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("/pass/serviceLogin", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("User-Agent"), "APP/xiaomi.smarthome") {
			http.Error(w, "ua", 400)
			return
		}
		switch r.URL.Query().Get("sid") {
		case "mijia":
			fmt.Fprint(w, `&&&START&&&{"code":70016,"_sign":"S","qs":"%3Fsid","callback":"https://sts/cb"}`)
		case "micoapi":
			ck, _ := r.Cookie("passToken")
			if ck == nil || ck.Value != "PT" || !f.passValid.Load() {
				fmt.Fprint(w, `&&&START&&&{"code":70016,"desc":"login required","_sign":"S2","qs":"%3Fsid%3Dmicoapi","callback":"https://sts/cb2","sid":"micoapi"}`)
				return
			}
			f.exchangeHits.Add(1)
			if f.holdExchange != nil {
				select {
				case <-f.holdExchange:
				case <-r.Context().Done():
					return
				}
			}
			fmt.Fprintf(w, `&&&START&&&{"code":0,"userId":12345678,"ssecurity":"SEC","nonce":%s,"location":"%s/sts?d=1"}`, nonce, f.srv.URL)
		}
	})
	mux.HandleFunc("/longPolling/loginUrl", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `&&&START&&&{"code":0,"qr":"%s/qr.png","loginUrl":"https://login","lp":"%s/lp","timeout":60}`, f.srv.URL, f.srv.URL)
	})
	mux.HandleFunc("/qr.png", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "d=not-an-image")
	})
	mux.HandleFunc("/lp", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-f.scanned:
			fmt.Fprint(w, `&&&START&&&{"code":0,"passToken":"PT","userId":"12345678","cUserId":"CU"}`)
		case <-time.After(200 * time.Millisecond):
			fmt.Fprint(w, ``) // keep-alive with no news
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/sts", func(w http.ResponseWriter, r *http.Request) {
		sum := sha1.Sum([]byte("nonce=" + nonce + "&SEC"))
		if r.URL.Query().Get("clientSign") != base64.StdEncoding.EncodeToString(sum[:]) {
			http.Error(w, "bad sign", 403)
			return
		}
		tok := fmt.Sprintf("ST%d", f.tokenSerial.Add(1))
		f.acceptToken.Store(tok)
		http.SetCookie(w, &http.Cookie{Name: "serviceToken", Value: tok})
		http.Redirect(w, r, "/done", http.StatusFound)
	})
	mux.HandleFunc("/pass/serviceLoginAuth2", func(w http.ResponseWriter, r *http.Request) {
		f.pwSubmits.Add(1)
		if r.Method != http.MethodPost || r.ParseForm() != nil {
			http.Error(w, "method", 405)
			return
		}
		if dev, _ := r.Cookie("deviceId"); dev == nil || r.Form.Get("_sign") != "S2" || r.Form.Get("callback") != "https://sts/cb2" {
			http.Error(w, "missing login params", 400)
			return
		}
		sum := md5.Sum([]byte("secret-pw"))
		if r.Form.Get("user") != "me@example.com" || r.Form.Get("hash") != strings.ToUpper(hex.EncodeToString(sum[:])) {
			fmt.Fprint(w, `&&&START&&&{"code":70016,"description":"密码错误"}`)
			return
		}
		if f.pwFlow == "captcha" {
			ick, _ := r.Cookie("ick")
			if r.Form.Get("captCode") != "ABCD" || ick == nil || ick.Value != "ICK" {
				fmt.Fprint(w, `&&&START&&&{"code":87001,"captchaUrl":"/pass/getCode?icodeType=login"}`)
				return
			}
		}
		if f.pwFlow == "verify" {
			fmt.Fprint(w, `&&&START&&&{"code":0,"notificationUrl":"/fe/service/identity/authStart?sid=micoapi&context=C"}`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "passToken", Value: "PT", Path: "/"})
		fmt.Fprintf(w, `&&&START&&&{"code":0,"userId":12345678,"cUserId":"CU","passToken":"PT","ssecurity":"SEC","nonce":%s,"location":"%s/sts?d=1"}`, nonce, f.srv.URL)
	})
	mux.HandleFunc("/pass/getCode", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "ick", Value: "ICK", Path: "/"})
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("JPEG"))
	})
	mux.HandleFunc("/identity/list", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "identity_session", Value: "IS", Path: "/"})
		fmt.Fprint(w, `&&&START&&&{"code":0,"flag":4}`)
	})
	mux.HandleFunc("/identity/auth/verifyPhone", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		is, _ := r.Cookie("identity_session")
		if is == nil || r.Form.Get("_flag") != "4" || r.Form.Get("ticket") != "123456" {
			fmt.Fprint(w, `&&&START&&&{"code":70014,"tips":"验证码错误"}`)
			return
		}
		fmt.Fprintf(w, `&&&START&&&{"code":0,"location":"%s/pass/afterVerify"}`, f.srv.URL)
	})
	mux.HandleFunc("/pass/afterVerify", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "passToken", Value: "PT", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "userId", Value: "12345678", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "cUserId", Value: "CU", Path: "/"})
		http.Redirect(w, r, "/done", http.StatusFound)
	})
	mux.HandleFunc("/device_profile/v2/conversation", func(w http.ResponseWriter, r *http.Request) {
		ck, _ := r.Cookie("serviceToken")
		dev, _ := r.Cookie("deviceId")
		if ck == nil || ck.Value != f.acceptToken.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if dev == nil || dev.Value != "d1" || r.URL.Query().Get("hardware") != "LX06" {
			http.Error(w, "bad device", 400)
			return
		}
		fmt.Fprint(w, `{"code":0,"message":"Success","data":"{\"records\":[{\"requestId\":\"R1\",\"time\":1800000000123,\"query\":\"播放晴天\",\"answers\":[{\"type\":\"TTS\",\"tts\":{\"text\":\"好的\"}},{\"type\":\"AUDIO\"}]}]}"}`)
	})
	mux.HandleFunc("/remote/ubus", func(w http.ResponseWriter, r *http.Request) {
		ck, _ := r.Cookie("serviceToken")
		if ck == nil || ck.Value != f.acceptToken.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.ParseForm()
		f.ubusMu.Lock()
		f.ubusCalls = append(f.ubusCalls, r.PostForm)
		f.ubusMu.Unlock()
		switch r.PostForm.Get("method") {
		case "player_get_play_status":
			fmt.Fprint(w, `{"code":0,"data":{"code":0,"info":"{\"status\":1,\"volume\":35,\"play_song_detail\":{\"audio_id\":\"987654321987654321\",\"position\":4200,\"duration\":200000}}"}}`)
		case "player_set_volume":
			fmt.Fprint(w, `{"code":0,"data":{"code":123}}`) // speaker refuses
		default:
			fmt.Fprint(w, `{"code":0,"data":{"code":0}}`)
		}
	})
	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/admin/v2/device_list", func(w http.ResponseWriter, r *http.Request) {
		variant := "plain"
		if r.URL.Query().Get("master") == "1" {
			variant = "master"
		}
		n, _ := f.listCalls.LoadOrStore(variant, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		ck, _ := r.Cookie("serviceToken")
		if ck == nil || ck.Value != f.acceptToken.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if variant == "plain" && f.plainEmpty {
			fmt.Fprint(w, `{"code":0,"data":[]}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":[{"deviceID":"d1","name":"书房","alias":"书房音箱","hardware":"LX06","presence":"online","miotDID":"9"}]}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) calls(variant string) int32 {
	n, ok := f.listCalls.Load(variant)
	if !ok {
		return 0
	}
	return n.(*atomic.Int32).Load()
}

type sinkRec struct {
	mu   sync.Mutex
	devs []Device
}

func (s *sinkRec) sink(_ context.Context, d []Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devs = d
	return nil
}

func newManager(t *testing.T, f *fake, path string) (*Manager, *sinkRec) {
	t.Helper()
	rec := &sinkRec{}
	m, err := NewManager(NewClient(Endpoints{Account: f.srv.URL, Mina: f.srv.URL, UserProfile: f.srv.URL}), path, rec.sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return m, rec
}

func waitState(t *testing.T, m *Manager, want string) QRStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := m.QR(); ok && st.State == want {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := m.QR()
	t.Fatalf("QR state = %+v, want %s", st, want)
	return st
}

func TestQRLoginLinksAccountAndFindsSpeakersViaMasterVariant(t *testing.T) {
	f := newFake(t)
	f.plainEmpty = true
	path := filepath.Join(t.TempDir(), "private", "xiaomi.json")
	m, rec := newManager(t, f, path)

	st, err := m.StartQR(context.Background())
	if err != nil || st.State != QRPending {
		t.Fatalf("start = %+v, %v", st, err)
	}
	img, ct, err := m.QRImage(context.Background(), st.ID, false)
	if err != nil || ct != "image/png" {
		t.Fatal(err)
	}
	info, err := png.DecodeConfig(bytes.NewReader(img))
	if err != nil || info.Width != 320 || info.Height != 320 {
		t.Fatalf("invalid QR image: %+v, %v", info, err)
	}
	if _, _, err := m.QRImage(context.Background(), "older-attempt", false); KindOf(err) != KindExpired {
		t.Fatalf("image served for a different attempt: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // at least one empty long poll
	close(f.scanned)
	st = waitState(t, m, QRLinked)
	if st.Sync == nil || st.Sync.Outcome != SyncOK || st.Sync.Variant != "master" || st.Sync.Count != 1 {
		t.Fatalf("sync = %+v", st.Sync)
	}
	if len(rec.devs) != 1 || rec.devs[0].DeviceID != "d1" {
		t.Fatalf("sink got %+v", rec.devs)
	}
	acct := m.Status()
	if !acct.Linked || acct.UserID != "12****78" || acct.RelinkRequired {
		t.Fatalf("status = %+v", acct)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v %v", fi, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"pass_token":"PT"`) {
		t.Fatal("credentials not saved")
	}

	// A restarted process picks the account up from disk.
	m2, _ := newManager(t, f, path)
	if !m2.Status().Linked {
		t.Fatal("reload lost the account")
	}
}

func TestQRUsesOfficialImageUnlessBackupIsRequested(t *testing.T) {
	f := newFake(t)
	official, err := qrcode.Encode("https://login", qrcode.Low, 240)
	if err != nil {
		t.Fatal(err)
	}
	original := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/qr.png" {
			w.Write(official)
			return
		}
		original.ServeHTTP(w, r)
	})
	m, _ := newManager(t, f, filepath.Join(t.TempDir(), "xiaomi.json"))
	defer m.CancelQR()
	ctx := context.Background()
	st, err := m.StartQR(ctx)
	if err != nil {
		t.Fatal(err)
	}
	picture, ct, err := m.QRImage(ctx, st.ID, false)
	if err != nil || ct != "image/png" || !bytes.Equal(picture, official) {
		t.Fatal("valid official picture was replaced", ct, err)
	}
	backup, ct, err := m.QRImage(ctx, st.ID, true)
	want, _ := qrcode.Encode("https://login", qrcode.Medium, 320)
	if err != nil || ct != "image/png" || !bytes.Equal(backup, want) {
		t.Fatal("backup did not encode the same login URL", ct, err)
	}
	if current, _ := m.QR(); current.ID != st.ID || current.State != QRPending {
		t.Fatal("backup changed the pending attempt", current)
	}
}

func TestSyncRenewsRejectedServiceToken(t *testing.T) {
	f := newFake(t)
	path := filepath.Join(t.TempDir(), "xiaomi.json")
	m, rec := newManager(t, f, path)
	m.StartQR(context.Background())
	close(f.scanned)
	waitState(t, m, QRLinked)
	before := f.exchangeHits.Load()

	f.acceptToken.Store("rotated-elsewhere") // Mina now rejects our token
	info := m.Sync(context.Background())
	if info.Outcome != SyncOK || f.exchangeHits.Load() != before+1 || len(rec.devs) != 1 {
		t.Fatalf("sync = %+v, exchanges %d→%d", info, before, f.exchangeHits.Load())
	}
	if f.calls("master") != 0 {
		t.Fatal("master variant used although the default list answered")
	}
}

func TestSyncReportsRelinkWhenPassTokenRevoked(t *testing.T) {
	f := newFake(t)
	m, _ := newManager(t, f, filepath.Join(t.TempDir(), "xiaomi.json"))
	m.StartQR(context.Background())
	close(f.scanned)
	waitState(t, m, QRLinked)

	f.passValid.Store(false)
	f.acceptToken.Store("gone")
	info := m.Sync(context.Background())
	if info.Outcome != SyncRelinkRequired || info.ErrorKind != KindAuth {
		t.Fatalf("sync = %+v", info)
	}
	if !m.Status().RelinkRequired {
		t.Fatal("status should ask for a new scan")
	}

	// It works after all: no scan needed.
	f.passValid.Store(true)
	m.syncMu.Lock()
	m.triedErr = nil
	m.syncMu.Unlock()
	if info := m.Sync(context.Background()); info.Outcome != SyncOK || m.Status().RelinkRequired {
		t.Fatalf("sync = %+v, relink %v", info, m.Status().RelinkRequired)
	}
}

func TestEmptyAccountIsReportedAsEmpty(t *testing.T) {
	f := newFake(t)
	f.plainEmpty = true
	m, _ := newManager(t, f, filepath.Join(t.TempDir(), "xiaomi.json"))
	m.StartQR(context.Background())
	close(f.scanned)
	waitState(t, m, QRLinked)
	// Make both variants empty.
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":[]}`)
	})
	info := m.Sync(context.Background())
	if info.Outcome != SyncEmpty || info.Count != 0 {
		t.Fatalf("sync = %+v", info)
	}
}

func TestUnlinkRemovesCredentials(t *testing.T) {
	f := newFake(t)
	path := filepath.Join(t.TempDir(), "xiaomi.json")
	m, _ := newManager(t, f, path)
	m.StartQR(context.Background())
	close(f.scanned)
	waitState(t, m, QRLinked)
	if err := m.Unlink(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credentials still present: %v", err)
	}
	if m.Status().Linked {
		t.Fatal("still linked")
	}
}

func TestExpiredPollEndsLogin(t *testing.T) {
	f := newFake(t)
	f.srv.Config.Handler = func(orig http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/lp" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			orig.ServeHTTP(w, r)
		})
	}(f.srv.Config.Handler)
	m, _ := newManager(t, f, filepath.Join(t.TempDir(), "xiaomi.json"))
	st, err := m.StartQR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, QRExpired)
	if _, _, err := m.QRImage(context.Background(), st.ID, false); KindOf(err) != KindExpired {
		t.Fatalf("image after expiry: %v", err)
	}
}

func TestConversationsParsesRecordsAndRenewsToken(t *testing.T) {
	f := newFake(t)
	m, _ := newManager(t, f, filepath.Join(t.TempDir(), "xiaomi.json"))
	if _, err := m.Conversations(context.Background(), "d1", "LX06", 5); err != ErrNotLinked {
		t.Fatalf("unlinked: %v", err)
	}
	m.StartQR(context.Background())
	close(f.scanned)
	waitState(t, m, QRLinked)
	before := f.exchangeHits.Load()
	f.acceptToken.Store("rotated-elsewhere")
	recs, err := m.Conversations(context.Background(), "d1", "LX06", 5)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records %+v, %v", recs, err)
	}
	r := recs[0]
	if r.RequestID != "R1" || r.Time != 1800000000123 || r.Query != "播放晴天" || r.Answer != "好的" || len(r.AnswerTypes) != 2 || r.AnswerTypes[1] != "AUDIO" {
		t.Fatalf("record %+v", r)
	}
	if f.exchangeHits.Load() != before+1 {
		t.Fatal("token was not renewed")
	}
}
