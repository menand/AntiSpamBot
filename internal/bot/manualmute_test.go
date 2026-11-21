package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestManualMuteSuppressesOldChecksAfterRestart(t *testing.T) {
	t.Run("durable-only captcha after failed punishment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mute.db")
		b, fc := replyAnchorBot(t, path)
		ctx := b.runCtx
		serviceableChat(t, b, b.db, testChatID)
		p := b.store.Put(testChatID, testUserID, 77, 0, time.Now().Add(-time.Minute), 0, 0, captchaStages)
		if err := b.db.PutPending(ctx, storage.PendingRow{ChatID: testChatID, UserID: testUserID,
			MessageID: 77, ExpiresAt: p.ExpiresAt, Stage: p.Stage}); err != nil {
			t.Fatal(err)
		}
		fc.err["banChatMember"] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}
		if next := b.advanceCaptchaStage(ctx, testChatID, testUserID, p); next != nil {
			t.Fatal("failed punishment returned a new timer")
		}
		if _, ok := b.store.Get(testChatID, testUserID); ok || len(pendingRows(t, b.db)) != 1 {
			t.Fatal("expected a captcha present only in SQLite")
		}
		if err := b.handleMuteCommand(nil, muteRightsCommand(testChatID, testUserID)); err != nil {
			t.Fatal(err)
		}
		if err := b.db.Close(); err != nil {
			t.Fatal(err)
		}
		restarted, recovered := replyAnchorBot(t, path)
		assertMuteRecoveryQuiescent(t, restarted)
		if calls := recovered.callList(); len(calls) != 0 {
			t.Fatalf("confirmed mute replayed an older action: %v", calls)
		}
	})

	t.Run("stop after Telegram success before local confirmation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mute.db")
		b, fc := replyAnchorBot(t, path)
		ctx := b.runCtx
		serviceableChat(t, b, b.db, testChatID)
		putCaptcha(b, b.db, testChatID, testUserID, 77)
		if err := b.db.SetWelcomeMode(ctx, testChatID, string(welcomeReply)); err != nil {
			t.Fatal(err)
		}
		if err := b.db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: testChatID, UserID: testUserID, Stage: 2, ExpiresAt: time.Now().Add(-time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		crash := new(int)
		setMuteTestCaller(t, b, fc, func(data *telegoapi.RequestData, r *telegoapi.Response, err error) (*telegoapi.Response, error) {
			m, pending, readErr := b.db.GetManualMute(ctx, testChatID, testUserID)
			if readErr != nil || !pending || m.UntilDate != muteRequestUntil(t, data) || err != nil || !r.Ok {
				t.Fatalf("mute must be saved before successful Telegram mutation: %+v, %v, %v, %v", m, pending, readErr, err)
			}
			panic(crash)
		})
		func() {
			defer func() {
				if recovered := recover(); recovered != crash {
					t.Errorf("want simulated crash after Telegram success, got %v", recovered)
				}
			}()
			_ = b.handleMuteCommand(nil, muteRightsCommand(testChatID, testUserID))
		}()
		m, pending, err := b.db.GetManualMute(ctx, testChatID, testUserID)
		if err != nil || !pending || len(pendingRows(t, b.db)) != 1 {
			t.Fatalf("must have both mute and old checkpoint at crash: %+v, %v, %v", m, pending, err)
		}
		if err := b.db.Close(); err != nil {
			t.Fatal(err)
		}
		restarted, recovered := replyAnchorBot(t, path)
		recovered.resp["getChatMember"] = fmt.Sprintf(`{"status":"restricted","is_member":true,"until_date":%d,"user":{"id":7}}`, m.UntilDate)
		assertMuteRecoveryQuiescent(t, restarted)
		if recovered.callCount("getChatMember") != 1 || recovered.callCount("restrictChatMember") != 0 {
			t.Fatalf("already applied mute should be confirmed without repeating rights: %v", recovered.callList())
		}
	})
}

func TestManualMuteDefiniteRejectionKeepsCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mute.db")
	b, fc := replyAnchorBot(t, path)
	serviceableChat(t, b, b.db, testChatID)
	putCaptcha(b, b.db, testChatID, testUserID, 77)
	fc.err["restrictChatMember"] = &telegoapi.Error{ErrorCode: 403, Description: "no rights"}
	if err := b.handleMuteCommand(nil, muteRightsCommand(testChatID, testUserID)); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := b.db.GetManualMute(b.runCtx, testChatID, testUserID); err != nil || pending {
		t.Fatalf("rejected mute left a decision: %v, %v", pending, err)
	}
	if p, ok := b.store.Get(testChatID, testUserID); !ok || p.MessageID != 77 {
		t.Fatal("definite rejection lost the live captcha")
	}
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, recovered := replyAnchorBot(t, path)
	restarted.reconcileManualMutes(restarted.runCtx)
	if n, err := restarted.restorePending(restarted.runCtx); n != 1 || err != nil {
		t.Fatalf("previous captcha must be restored after rejection: %d, %v", n, err)
	}
	if recovered.callCount("restrictChatMember") != 0 {
		t.Fatal("definitely rejected request was repeated")
	}
}

func TestUncertainManualMuteRetriesOriginalDeadline(t *testing.T) {
	shortAdmissionRetries(t)
	path := filepath.Join(t.TempDir(), "mute.db")
	b, fc := replyAnchorBot(t, path)
	serviceableChat(t, b, b.db, testChatID)
	putCaptcha(b, b.db, testChatID, testUserID, 77)
	attempts := 0
	var until int64
	setMuteTestCaller(t, b, fc, func(data *telegoapi.RequestData, _ *telegoapi.Response, _ error) (*telegoapi.Response, error) {
		attempts++
		got := muteRequestUntil(t, data)
		if attempts == 1 {
			until = got
			return nil, io.ErrUnexpectedEOF
		}
		if got != until {
			t.Fatalf("retry extended mute from %d to %d", until, got)
		}
		denied := &telegoapi.Error{ErrorCode: 403, Description: "no rights"}
		return &telegoapi.Response{Ok: false, Error: denied}, denied
	})
	if err := b.handleMuteCommand(nil, muteRightsCommand(testChatID, testUserID)); err != nil {
		t.Fatal(err)
	}
	if m, pending, err := b.db.GetManualMute(b.runCtx, testChatID, testUserID); err != nil || !pending || m.UntilDate != until {
		t.Fatalf("mixed failure lost original intent: %+v, %v, %v", m, pending, err)
	}
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, recovered := replyAnchorBot(t, path)
	if n, err := restarted.restorePending(restarted.runCtx); n != 0 || err != nil {
		t.Fatalf("unresolved mute must suspend old captcha: %d, %v", n, err)
	}
	assertMuteRecoveryQuiescent(t, restarted)
	bodies := recovered.callBodies("restrictChatMember")
	if len(bodies) != 1 || !strings.Contains(bodies[0], fmt.Sprintf(`"until_date":%d`, until)) {
		t.Fatalf("restart did not retry the original deadline: %v", bodies)
	}
}

func TestManualMuteRecoverySkipsShortDeadline(t *testing.T) {
	for _, remaining := range []time.Duration{-time.Second, 20 * time.Second} {
		t.Run(remaining.String(), func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			ctx := b.runCtx
			serviceableChat(t, b, db, testChatID)
			putCaptcha(b, db, testChatID, testUserID, 77)
			if err := db.PutManualMute(ctx, storage.ManualMute{ChatID: testChatID, UserID: testUserID,
				Generation: "expired", UntilDate: time.Now().Add(remaining).Unix()}); err != nil {
				t.Fatal(err)
			}
			assertMuteRecoveryQuiescent(t, b)
			if n := fc.callCount("restrictChatMember"); n != 0 {
				t.Fatalf("issued a potentially permanent restriction: %v", fc.callList())
			}
		})
	}
}

func TestManualMuteRecoveryRespectsExistingRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name, restriction    string
		remaining            time.Duration
		failStatus           bool
		failRelease          bool
		changedBeforeRelease bool
		wantRelease          bool
	}{
		{name: "expired legacy captcha", restriction: "captcha", remaining: -time.Second, wantRelease: true},
		{name: "short deadline legacy captcha", restriction: "captcha", remaining: 20 * time.Second, wantRelease: true},
		{name: "newer mute", restriction: "foreign", remaining: -time.Second},
		{name: "newer mute before deadline", restriction: "foreign", remaining: 2 * time.Minute},
		{name: "confirmed short mute", restriction: "requested", remaining: 20 * time.Second},
		{name: "confirmed expired mute", restriction: "requested", remaining: -time.Second},
		{name: "active quarantine", restriction: "quarantine", remaining: -time.Second},
		{name: "membership lookup retry", restriction: "captcha", remaining: -time.Second, failStatus: true, wantRelease: true},
		{name: "release retry", restriction: "captcha", remaining: -time.Second, failRelease: true, wantRelease: true},
		{name: "new mute arrives before release", restriction: "captcha", remaining: -time.Second, changedBeforeRelease: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortAdmissionRetries(t)
			path := filepath.Join(t.TempDir(), "mute.db")
			b, _ := replyAnchorBot(t, path)
			ctx := b.runCtx
			serviceableChat(t, b, b.db, testChatID)
			putCaptcha(b, b.db, testChatID, testUserID, 77)
			m := storage.ManualMute{ChatID: testChatID, UserID: testUserID, Generation: "interrupted", UntilDate: time.Now().Add(tc.remaining).Unix()}
			if err := b.db.PutManualMute(ctx, m); err != nil {
				t.Fatal(err)
			}
			until := int64(0)
			switch tc.restriction {
			case "foreign":
				until = time.Now().Add(time.Hour).Unix()
			case "requested":
				until = m.UntilDate
			case "quarantine":
				until = time.Now().Add(time.Hour).Unix()
				if err := b.db.SetQuarantineEnabled(ctx, testChatID, true); err != nil {
					t.Fatal(err)
				}
				if err := b.db.PutQuarantine(ctx, testChatID, testUserID, time.Unix(until, 0), true); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.db.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, fc := replyAnchorBot(t, path)
			ctx = restarted.runCtx
			fc.resp["getChat"] = `{"permissions":{"can_send_messages":true}}`
			fc.resp["getChatMember"] = fmt.Sprintf(`{"status":"restricted","is_member":true,"until_date":%d,"can_send_messages":%t,"user":{"id":7}}`, until, tc.restriction == "quarantine")
			if tc.changedBeforeRelease {
				newer := fmt.Sprintf(`{"status":"restricted","is_member":true,"until_date":%d,"user":{"id":7}}`, time.Now().Add(time.Hour).Unix())
				fc.respSeq["getChatMember"] = []string{fc.resp["getChatMember"], newer}
				restarted.reconcileManualMutes(ctx)
				if fc.callCount("restrictChatMember") != 0 {
					t.Fatalf("released a newer mute: %v", fc.callList())
				}
			}
			if tc.failStatus || tc.failRelease {
				method := "getChatMember"
				if tc.failRelease {
					method = "restrictChatMember"
				}
				fc.err[method] = &telegoapi.Error{ErrorCode: 502}
				restarted.reconcileManualMutes(ctx)
				if _, found, err := restarted.db.GetManualMute(ctx, testChatID, testUserID); err != nil || !found || len(pendingRows(t, restarted.db)) != 1 {
					t.Fatalf("uncertain result lost its evidence: pending=%t / %v", found, err)
				}
				delete(fc.err, method)
			}
			before := fc.callCount("restrictChatMember")
			assertMuteRecoveryQuiescent(t, restarted)
			if got := fc.callCount("restrictChatMember") - before; (tc.wantRelease && got != 1) || (!tc.wantRelease && got != 0) {
				t.Fatalf("wrong rights mutation: %v", fc.callBodies("restrictChatMember"))
			}
			if tc.wantRelease && !strings.Contains(fc.callBodies("restrictChatMember")[before], `"can_send_messages":true`) {
				t.Fatalf("old captcha did not regain chat permissions: %v", fc.callBodies("restrictChatMember"))
			}
			if tc.restriction == "quarantine" {
				entry, found := restarted.quar.Get(testChatID, testUserID)
				if active, err := restarted.db.ActiveQuarantineUntil(ctx, testChatID, testUserID, time.Now().Unix()); err != nil || active != until || !found || !entry.Warned {
					t.Fatalf("active quarantine lost: %d / %v / %+v / %t", active, err, entry, found)
				}
			}
			if tc.restriction == "requested" {
				if users, err := restarted.db.RecentEventUsers(ctx, testChatID, 10, []storage.EventKind{storage.EventMute}, nil); err != nil || len(users) != 1 || users[0].UserID != testUserID {
					t.Fatalf("already applied short mute should be recorded: %+v / %v", users, err)
				}
			}
		})
	}
}

func assertMuteRecoveryQuiescent(t *testing.T, b *Bot) {
	t.Helper()
	ctx := b.runCtx
	b.reconcileManualMutes(ctx)
	if n, err := b.restorePending(ctx); n != 0 || err != nil {
		t.Fatalf("old captcha restored: %d, %v", n, err)
	}
	if n, err := b.restorePendingReplies(ctx); n != 0 || err != nil {
		t.Fatalf("old reply wait restored: %d, %v", n, err)
	}
	b.reconcileAdmissions(ctx)
	if rows := pendingRows(t, b.db); len(rows) != 0 {
		t.Fatalf("old captcha remains on disk: %+v", rows)
	}
	if rows, err := b.db.LoadAllPendingReplies(ctx); len(rows) != 0 || err != nil {
		t.Fatalf("old reply wait remains on disk: %+v, %v", rows, err)
	}
	if rows, err := b.db.LoadManualMutes(ctx); len(rows) != 0 || err != nil {
		t.Fatalf("mute intent was not settled: %+v, %v", rows, err)
	}
}

type muteTestCaller struct {
	*fakeCaller
	after func(*telegoapi.RequestData, *telegoapi.Response, error) (*telegoapi.Response, error)
}

func (m muteTestCaller) Call(ctx context.Context, url string, data *telegoapi.RequestData) (*telegoapi.Response, error) {
	r, err := m.fakeCaller.Call(ctx, url, data)
	if strings.HasSuffix(url, "/restrictChatMember") {
		return m.after(data, r, err)
	}
	return r, err
}

func setMuteTestCaller(t *testing.T, b *Bot, fc *fakeCaller, after func(*telegoapi.RequestData, *telegoapi.Response, error) (*telegoapi.Response, error)) {
	t.Helper()
	api, err := telego.NewBot("123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi",
		telego.WithAPICaller(muteTestCaller{fakeCaller: fc, after: after}), telego.WithDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	b.api = api
}

func muteRequestUntil(t *testing.T, data *telegoapi.RequestData) int64 {
	t.Helper()
	var r struct {
		Until int64 `json:"until_date"`
	}
	if err := json.Unmarshal(data.BodyRaw, &r); err != nil || r.Until == 0 {
		t.Fatalf("no finite until_date on manual mute request: %+v, %v", r, err)
	}
	return r.Until
}
