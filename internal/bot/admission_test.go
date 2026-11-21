package bot

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestLeaveChatWaitsForCaptchaAdmission(t *testing.T) {
	for _, when := range []string{"saved admission", "concurrent admission", "forced removal"} {
		t.Run(when, func(t *testing.T) {
			shortAdmissionRetries(t)
			b, db, fc := newFlowBot(t)
			ctx := b.runCtx
			serviceableChat(t, b, db, testChatID)
			p := putCaptcha(b, db, testChatID, testUserID, 77)
			q := telego.CallbackQuery{ID: "correct", From: telego.User{ID: testUserID},
				Data:    fmt.Sprintf("cap:%d:%d", testUserID, p.CorrectIdx),
				Message: &telego.Message{MessageID: 77, Chat: telego.Chat{ID: testChatID}}}
			fc.err["restrictChatMember"] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}
			leave := func() error {
				done := make(chan error, 1)
				b.leaveChatAndCleanup(testChatID, "test leave", func(err error) { done <- err })
				select {
				case err := <-done:
					return err
				case <-time.After(5 * time.Second):
					t.Fatal("leave result timed out")
					return nil
				}
			}
			if when == "concurrent admission" {
				entered, resume := make(chan struct{}), make(chan struct{})
				resumeAnswer := sync.OnceFunc(func() { close(resume) })
				defer resumeAnswer()
				var first sync.Once
				setMuteTestCaller(t, b, fc, func(_ *telegoapi.RequestData, r *telegoapi.Response, err error) (*telegoapi.Response, error) {
					first.Do(func() { close(entered); <-resume })
					return r, err
				})
				done := make(chan error, 1)
				go func() { done <- b.handleCallback(nil, q) }()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("answer did not reach Telegram before leave")
				}
				err := leave()
				resumeAnswer()
				if err == nil {
					t.Error("leave passed during the pending rights request")
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			} else if err := b.handleCallback(nil, q); err != nil {
				t.Fatal(err)
			}
			row, pending, err := db.GetPendingAdmission(ctx, testChatID, testUserID)
			if err != nil || !pending || !row.ReleaseNeeded || len(pendingRows(t, db)) != 0 {
				t.Fatalf("expected a pending release after the correct answer: %+v / %t / %v", row, pending, err)
			}
			if when == "forced removal" {
				update := telego.Update{MyChatMember: &telego.ChatMemberUpdated{
					Chat:          telego.Chat{ID: testChatID, Type: telego.ChatTypeSupergroup},
					OldChatMember: &telego.ChatMemberMember{User: *b.me}, NewChatMember: &telego.ChatMemberBanned{User: *b.me},
				}}
				if err := b.handleMyChatMember(nil, update); err != nil {
					t.Fatal(err)
				}
				if _, pending, err := db.GetPendingAdmission(ctx, testChatID, testUserID); err != nil || pending || fc.callCount("leaveChat") != 0 {
					t.Fatalf("forced removal did not clean the admission: %t / %v / %v", pending, err, fc.callList())
				}
				return
			}
			if when == "saved admission" {
				if err := leave(); err == nil {
					t.Error("leave passed while an admission still needed rights")
				}
			}
			if _, pending, err := db.GetPendingAdmission(ctx, testChatID, testUserID); err != nil || !pending || fc.callCount("leaveChat") != 0 {
				t.Fatalf("leave discarded the admission: %t / %v / %v", pending, err, fc.callList())
			}
			delete(fc.err, "restrictChatMember")
			b.reconcileAdmissions(ctx)
			if err := leave(); err != nil || fc.callCount("leaveChat") != 1 {
				t.Fatalf("leave failed after permissions were restored: %v / %v", err, fc.callList())
			}
		})
	}
}

func TestCaptchaAdmissionSurvivesRestart(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       welcomeMode
		crashAt    string
		quarantine bool
	}{
		{name: "остановка до снятия прав", mode: welcomeGreet, crashAt: "restrictChatMember"},
		{name: "восстановление текстового карантина", mode: welcomeGreet, crashAt: "restrictChatMember", quarantine: true},
		{name: "отказ снятия прав", mode: welcomeReply},
		{name: "неизвестный исход отправки приветствия", mode: welcomeReply, crashAt: "sendMessage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortAdmissionRetries(t)
			path := filepath.Join(t.TempDir(), "admission.db")
			b, fc := replyAnchorBot(t, path)
			ctx := b.runCtx
			serviceableChat(t, b, b.db, testChatID)
			if err := b.db.SetWelcomeMode(ctx, testChatID, string(tc.mode)); err != nil {
				t.Fatal(err)
			}
			if tc.quarantine {
				if err := b.db.SetQuarantineEnabled(ctx, testChatID, true); err != nil {
					t.Fatal(err)
				}
			}
			p := putCaptcha(b, b.db, testChatID, testUserID, 77)
			q := telego.CallbackQuery{
				ID: "correct", From: telego.User{ID: testUserID}, Data: fmt.Sprintf("cap:%d:%d", testUserID, p.CorrectIdx),
				Message: &telego.Message{MessageID: 77, Chat: telego.Chat{ID: testChatID}},
			}
			if tc.crashAt != "" {
				interruptAdmission(t, b, fc, tc.crashAt, func() { _ = b.handleCallback(nil, q) })
			} else {
				fc.err["restrictChatMember"] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}
				if err := b.handleCallback(nil, q); err != nil {
					t.Fatal(err)
				}
				if bodies := fc.callBodies("answerCallbackQuery"); len(bodies) != 1 || !strings.Contains(bodies[0], "Права ещё восстанавливаются") {
					t.Fatalf("partial result was not reported: %v", bodies)
				}
			}
			if got := pendingRows(t, b.db); len(got) != 0 {
				t.Fatalf("accepted captcha could be restored as an active check: %+v", got)
			}
			decision, pending, err := b.db.GetPendingAdmission(ctx, testChatID, testUserID)
			if err != nil || pending != (tc.crashAt != "sendMessage") {
				t.Fatalf("durable admission pending = %v, %v", pending, err)
			}
			if err := b.db.Close(); err != nil {
				t.Fatal(err)
			}

			restarted, recovered := replyAnchorBot(t, path)
			ctx = restarted.runCtx
			if n, err := restarted.restorePending(ctx); err != nil || n != 0 {
				t.Fatalf("old captcha rearmed = %d, %v", n, err)
			}
			if n, err := restarted.restorePendingReplies(ctx); err != nil || n != 0 {
				t.Fatalf("unexpected restored timer = %d, %v", n, err)
			}
			restarted.reconcileAdmissions(ctx)
			restarted.reconcileAdmissions(ctx)
			if err := restarted.handleCallback(nil, q); err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.crashAt == "sendMessage" {
				want = 0
			}
			if recovered.callCount("restrictChatMember") != want || recovered.callCount("sendMessage") != want ||
				recovered.callCount("banChatMember") != 0 {
				t.Fatalf("expected %d release and welcome, no punishment: %v", want, recovered.callList())
			}
			if tc.quarantine {
				wantUntil := fmt.Sprintf(`"until_date":%d`, decision.QuarantineUntil)
				body := recovered.callBodies("restrictChatMember")[0]
				if !strings.Contains(body, wantUntil) || !strings.Contains(body, `"can_send_messages":true`) ||
					!strings.Contains(body, `"can_add_web_page_previews":false`) {
					t.Fatalf("retry changed the selected quarantine: %s", body)
				}
				if active, err := restarted.db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix()); err != nil || !active {
					t.Fatalf("quarantine not saved: %v, %v", active, err)
				}
			}

			if _, pending, err := restarted.db.GetPendingAdmission(ctx, testChatID, testUserID); err != nil || pending {
				t.Fatalf("remaining admission = %v, %v", pending, err)
			}
			if tc.mode == welcomeReply && want == 1 {
				rows, err := restarted.db.LoadAllPendingReplies(ctx)
				if err != nil || len(rows) != 1 || rows[0].GreetingMsgID != 555 || rows[0].WelcomeClaimed {
					t.Fatalf("reply wait is not bound to one welcome: %+v, %v", rows, err)
				}
				if k := statsKinds(t, restarted.db, testChatID, testUserID); k[storage.EventPass] != 0 {
					t.Fatalf("pass before reply: %v", k)
				}
				restarted.replyWaitSatisfied(testChatID, testUserID)
			}
			assertReplyAnchorPassed(t, restarted)
		})
	}
}

func TestManualAdmissionRetriesAfterPartialSuccess(t *testing.T) {
	for action, storedAction := range map[string]string{
		"u": storage.AdmissionLocalUnban, "w": storage.AdmissionWhitelist,
	} {
		t.Run(action, func(t *testing.T) {
			shortAdmissionRetries(t)
			path := filepath.Join(t.TempDir(), "admission.db")
			b, fc := replyAnchorBot(t, path)
			serviceableChat(t, b, b.db, testChatID)
			putCaptcha(b, b.db, testChatID, testUserID, 77)
			fc.err["restrictChatMember"] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}
			q := telego.CallbackQuery{
				ID: "unmod", From: telego.User{ID: 200}, Data: fmt.Sprintf("mc:%s:%d", action, testUserID),
				Message: &telego.Message{MessageID: 80, Chat: telego.Chat{ID: testChatID}},
			}
			if err := b.handleModChoiceCallback(nil, q); err != nil {
				t.Fatal(err)
			}
			ctx := b.runCtx
			row, ok, err := b.db.GetPendingAdmission(ctx, testChatID, testUserID)
			if err != nil || !ok || row.Action != storedAction || !row.UnbanDone || !row.ReleaseNeeded {
				t.Fatalf("manual admission did not record partial success: %+v, %t, %v", row, ok, err)
			}
			if got := fc.callBodies("answerCallbackQuery"); len(got) != 1 || !strings.Contains(got[0], "Разбан подтверждён") || !strings.Contains(got[0], "бот повторит запрос") {
				t.Fatalf("admin was not told about pending permissions: %v", got)
			}
			trusted, err := b.db.IsTrusted(ctx, testChatID, testUserID)
			if err != nil || trusted != (action == storage.AdmissionWhitelist) {
				t.Fatalf("trusted = %v, %v", trusted, err)
			}
			if err := b.db.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, recovered := replyAnchorBot(t, path)
			ctx = restarted.runCtx
			if n, err := restarted.restorePending(ctx); n != 0 || err != nil {
				t.Fatalf("old captcha restored = %d, %v", n, err)
			}
			restarted.reconcileAdmissions(ctx)
			restarted.reconcileAdmissions(ctx)
			if recovered.callCount("restrictChatMember") != 1 || recovered.callCount("unbanChatMember") != 0 {
				t.Fatalf("retry must release rights without repeating the confirmed unban: %v", recovered.callList())
			}
			assertReplyAnchorPassed(t, restarted)
		})
	}
}

func TestAdmissionRespectsNewPermanentBan(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ban          string
		banErrors    []*telegoapi.Error
		interrupt    bool
		statusChecks int
		newUnban     bool
		wantUnbans   int
	}{
		{name: "manual ban", ban: "manual"},
		{name: "accepted spam verdict", ban: "verdict"},
		{name: "direct spam report", ban: "direct"},
		{name: "cross-chat ban", ban: "cross"},
		{name: "confirmed refusal", ban: "manual", banErrors: []*telegoapi.Error{{ErrorCode: 403}}, wantUnbans: 1},
		{name: "uncertain then denied", ban: "manual", banErrors: []*telegoapi.Error{{ErrorCode: 502}, {ErrorCode: 403}}, statusChecks: 2},
		{name: "uncertain and no ban applied", ban: "manual", banErrors: []*telegoapi.Error{{ErrorCode: 502}}, statusChecks: 1, wantUnbans: 1},
		{name: "interrupted after successful ban", ban: "manual", interrupt: true, statusChecks: 2},
		{name: "new explicit unban", ban: "manual", banErrors: []*telegoapi.Error{{ErrorCode: 502}}, newUnban: true, wantUnbans: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortAdmissionRetries(t)
			old := kickUnbanBackoffs
			kickUnbanBackoffs = []time.Duration{0}
			t.Cleanup(func() { kickUnbanBackoffs = old })
			path := filepath.Join(t.TempDir(), "admission.db")
			b, fc := replyAnchorBot(t, path)
			serviceableChat(t, b, b.db, testChatID)
			fc.err["unbanChatMember"] = &telegoapi.Error{ErrorCode: 502}
			if _, err := b.execUnmod(storage.AdmissionUnban, testChatID, testUserID, nil); err == nil {
				t.Fatal("expected failed unban")
			}
			if _, ok, err := b.db.GetPendingAdmission(b.runCtx, testChatID, testUserID); err != nil || !ok {
				t.Fatalf("failed unban was not saved: %v / %v", ok, err)
			}
			if _, ok, err := b.db.GetPunishment(b.runCtx, testChatID, testUserID); err != nil || ok {
				t.Fatalf("scenario should exercise a ban without pending punishment: %v / %v", ok, err)
			}
			if len(tc.banErrors) > 0 {
				fc.errSeq["banChatMember"] = tc.banErrors
				fc.err["banChatMember"] = tc.banErrors[len(tc.banErrors)-1]
			}
			switch tc.ban {
			case "manual":
				run := func() {
					msg := telego.Message{MessageID: 99, Text: "/ban",
						Chat: telego.Chat{ID: testChatID, Type: telego.ChatTypeSupergroup}, From: &telego.User{ID: 200},
						ReplyToMessage: &telego.Message{MessageID: 98, From: &telego.User{ID: testUserID}}}
					if err := b.handleBanCommand(nil, msg); err != nil {
						t.Fatal(err)
					}
				}
				if tc.interrupt {
					interruptAfterKickBan(t, b, fc, run)
				} else {
					run()
				}
			case "verdict":
				b.acceptTalliedSpamVerdict(createVerdictVote(t, b), 1)
			case "direct":
				if banned, _ := b.executeSpamBan(storage.SpamVote{ChatID: testChatID, AuthorID: testUserID}, "test", "test"); !banned {
					t.Fatal("direct spam ban failed")
				}
			case "cross":
				const origin = testChatID - 1
				serviceableChat(t, b, b.db, origin)
				verdict := seedConfirmedCrossBan(t, b, origin, testChatID)
				if got, err := b.banEverywhere(b.runCtx, verdict); err != nil || len(got) != 1 {
					t.Fatalf("cross-chat ban did not reach its destination: %v / %v", got, err)
				}
			}
			if fc.callCount("banChatMember") == 0 || fc.callCount("unbanChatMember") != 1 {
				t.Fatalf("expected failed unban followed by ban: %v", fc.callList())
			}
			if err := b.db.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, recovered := replyAnchorBot(t, path)
			if tc.statusChecks == 2 {
				recovered.errSeq["getChatMember"] = []*telegoapi.Error{{ErrorCode: 502}}
				recovered.resp["getChatMember"] = fmt.Sprintf(`{"status":"kicked","until_date":0,"user":{"id":%d}}`, testUserID)
			}
			if tc.newUnban {
				if _, err := restarted.execUnmod(storage.AdmissionUnban, testChatID, testUserID, nil); err != nil {
					t.Fatalf("explicit new unban after an uncertain ban: %v", err)
				}
			}
			restarted.reconcileAdmissions(restarted.runCtx)
			if tc.statusChecks == 2 {
				if _, ok, err := restarted.db.GetPendingAdmission(restarted.runCtx, testChatID, testUserID); err != nil || !ok {
					t.Errorf("failed status check must keep the admission pending: %v / %v", ok, err)
				}
				if recovered.callCount("unbanChatMember") != 0 {
					t.Errorf("unban ran before the new ban was reconciled: %v", recovered.callList())
				}
			}
			restarted.reconcileAdmissions(restarted.runCtx)
			if got := recovered.callCount("getChatMember"); got != tc.statusChecks {
				t.Errorf("membership checks = %d, want %d: %v", got, tc.statusChecks, recovered.callList())
			}
			if got := recovered.callCount("unbanChatMember"); got != tc.wantUnbans {
				t.Errorf("unban retries after new ban = %d, want %d: %v", got, tc.wantUnbans, recovered.callList())
			}
			if _, ok, err := restarted.db.GetPendingAdmission(restarted.runCtx, testChatID, testUserID); err != nil || ok {
				t.Errorf("admission did not settle: %v / %v", ok, err)
			}
		})
	}
}

func shortAdmissionRetries(t *testing.T) {
	t.Helper()
	old := tgBackoffs
	tgBackoffs = []time.Duration{0, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { tgBackoffs = old })
}

func interruptAdmission(t *testing.T, b *Bot, fc *fakeCaller, method string, handle func()) {
	t.Helper()
	stop := new(int)
	caller := interruptedCaptchaCaller{fakeCaller: fc, before: func(url string, _ *telegoapi.RequestData) {
		if strings.HasSuffix(url, "/"+method) {
			panic(stop)
		}
	}}
	api, err := telego.NewBot("123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi", telego.WithAPICaller(caller), telego.WithDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	b.api = api
	defer func() {
		if crashed := recover(); crashed != stop {
			t.Errorf("want interruption at %s, got %v", method, crashed)
		}
	}()
	handle()
}

func TestManualCaptchaApprovalSavesDecisionBeforeRelease(t *testing.T) {
	b, db, fc := newFlowBot(t)
	shortAdmissionRetries(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{200: {}}
	putCaptcha(b, db, testChatID, testUserID, 77)
	fc.err["restrictChatMember"] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}
	q := telego.CallbackQuery{ID: "approve", From: telego.User{ID: 200}, Data: fmt.Sprintf("capok:%d", testUserID),
		Message: &telego.Message{MessageID: 77, Chat: telego.Chat{ID: testChatID}}}
	if err := b.handleApproveCallback(nil, q); err != nil {
		t.Fatal(err)
	}
	row, pending, err := db.GetPendingAdmission(context.Background(), testChatID, testUserID)
	if err != nil || !pending || row.Action != storage.AdmissionCaptcha || len(pendingRows(t, db)) != 0 {
		t.Fatalf("manual approval was not saved: %+v, %v, %v", row, pending, err)
	}
}
