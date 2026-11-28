package xiaomi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPasswordLoginLinksDirectly(t *testing.T) {
	f := newFake(t)
	f.pwFlow = "direct"
	path := filepath.Join(t.TempDir(), "xiaomi.json")
	m, rec := newManager(t, f, path)
	st, err := m.StartPassword(context.Background(), " me@example.com ", "secret-pw")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != PwStateLinked || st.Sync == nil || st.Sync.Outcome != SyncOK || len(rec.devs) != 1 {
		t.Fatalf("status %+v devices %v", st, rec.devs)
	}
	if st.Account != "****@example.com" {
		t.Fatalf("account not masked: %q", st.Account)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "secret-pw") || !strings.Contains(string(raw), `"pass_token":"PT"`) {
		t.Fatalf("credentials file: %s", raw)
	}
	if _, ok := m.Password(); ok {
		t.Fatal("finished attempt should be forgotten")
	}
}

func TestPasswordLoginWrongPassword(t *testing.T) {
	f := newFake(t)
	f.pwFlow = "direct"
	m, _ := newManager(t, f, filepath.Join(t.TempDir(), "x.json"))
	_, err := m.StartPassword(context.Background(), "me@example.com", "nope")
	if err == nil || KindOf(err) != KindAuth || !strings.Contains(err.Error(), "70016") {
		t.Fatalf("err = %v", err)
	}
	if m.Status().Linked {
		t.Fatal("must not link")
	}
}

func TestPasswordLoginCaptcha(t *testing.T) {
	f := newFake(t)
	f.pwFlow = "captcha"
	m, _ := newManager(t, f, filepath.Join(t.TempDir(), "x.json"))
	ctx := context.Background()
	st, err := m.StartPassword(ctx, "me@example.com", "secret-pw")
	if err != nil || st.State != PwStateCaptcha || st.CaptchaRev != 1 {
		t.Fatalf("start: %+v %v", st, err)
	}
	img, ct, err := m.PasswordCaptcha()
	if err != nil || string(img) != "JPEG" || ct != "image/jpeg" {
		t.Fatalf("captcha image: %q %q %v", img, ct, err)
	}
	if st, err = m.RefreshCaptcha(ctx); err != nil || st.CaptchaRev != 2 {
		t.Fatalf("refresh: %+v %v", st, err)
	}
	st, err = m.SubmitCaptcha(ctx, "WRONG")
	if err != nil || st.State != PwStateCaptcha || st.CaptchaRev != 3 || !strings.Contains(st.Message, "不对") {
		t.Fatalf("wrong captcha: %+v %v", st, err)
	}
	st, err = m.SubmitCaptcha(ctx, " ABCD ")
	if err != nil || st.State != PwStateLinked {
		t.Fatalf("right captcha: %+v %v", st, err)
	}
	if !m.Status().Linked {
		t.Fatal("not linked")
	}
}

func TestPasswordLoginPhoneVerification(t *testing.T) {
	f := newFake(t)
	f.pwFlow = "verify"
	m, rec := newManager(t, f, filepath.Join(t.TempDir(), "x.json"))
	ctx := context.Background()
	st, err := m.StartPassword(ctx, "13800001234", "secret-pw")
	if err == nil {
		t.Fatal("phone user is wrong in the fake; expected refusal")
	}
	st, err = m.StartPassword(ctx, "me@example.com", "secret-pw")
	if err != nil || st.State != PwStateVerify || st.VerifyType != "phone" || !strings.HasPrefix(st.VerifyURL, f.srv.URL+"/fe/service/identity/authStart") {
		t.Fatalf("start: %+v %v", st, err)
	}
	if _, err := m.SubmitCaptcha(ctx, "x"); err == nil {
		t.Fatal("captcha must be refused at the verify step")
	}
	st, err = m.SubmitVerifyCode(ctx, "000000")
	if err == nil || st.State != PwStateVerify {
		t.Fatalf("wrong code: %+v %v", st, err)
	}
	st, err = m.SubmitVerifyCode(ctx, "123456")
	if err != nil || st.State != PwStateLinked || len(rec.devs) != 1 {
		t.Fatalf("verify: %+v %v", st, err)
	}
	if _, err := m.SubmitVerifyCode(ctx, "123456"); err != ErrNoPasswordLogin {
		t.Fatalf("replay: %v", err)
	}
}

func TestMaskAccount(t *testing.T) {
	for in, want := range map[string]string{"13800001234": "138****1234", "me@example.com": "****@example.com", "alice@x.io": "al****ce@x.io", "123": "****"} {
		if got := maskAccount(in); got != want {
			t.Errorf("maskAccount(%q) = %q, want %q", in, got, want)
		}
	}
}

// Unlinking ends a password login that is still in progress: no step after it may resurrect
// the account, and the password hash does not linger.
func TestUnlinkEndsAPasswordLoginInProgress(t *testing.T) {
	f := newFake(t)
	f.pwFlow = "captcha"
	path := filepath.Join(t.TempDir(), "xiaomi.json")
	m, _ := newManager(t, f, path)
	ctx := context.Background()
	if _, err := m.StartPassword(ctx, "me@example.com", "secret-pw"); err != nil {
		t.Fatal(err)
	}
	if err := m.Unlink(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Password(); ok {
		t.Fatal("the attempt outlived the unlink, password hash and all")
	}
	if _, err := m.SubmitCaptcha(ctx, "ABCD"); err == nil {
		t.Fatal("a captcha submitted after the unlink resurrected the account")
	}
	if m.Status().Linked {
		t.Fatal("linked after the unlink")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("credentials file back", err)
	}
}

// A login that finishes after an Unlink does not write the account or its file back.
func TestUnlinkBeatsALoginThatIsFinishing(t *testing.T) {
	f := newFake(t)
	f.pwFlow = "captcha"
	path := filepath.Join(t.TempDir(), "xiaomi.json")
	m, _ := newManager(t, f, path)
	ctx := context.Background()
	f.holdExchange = make(chan struct{})
	if _, err := m.StartPassword(ctx, "me@example.com", "secret-pw"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.SubmitCaptcha(ctx, " ABCD ") // the captcha is right: the login links, but slowly
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for f.exchangeHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if f.exchangeHits.Load() == 0 {
		t.Fatal("the login never reached the exchange")
	}
	if err := m.Unlink(); err != nil {
		t.Fatal(err)
	}
	close(f.holdExchange)
	if err := <-done; !strings.Contains(err.Error(), ErrUnlinked.Detail) {
		t.Fatal("the finished login wrote the account back:", err)
	}
	if m.Status().Linked {
		t.Fatal("linked after the unlink")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("credentials file back", err)
	}
}
