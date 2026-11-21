package bot

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestKickRecoveryKeepsAttemptAndBanOwnership(t *testing.T) {
	for _, reason := range []string{storage.ReasonCaptcha, storage.ReasonNoReply} {
		for _, scenario := range []struct {
			name, fail           string
			crash, newer         bool
			wantBans, wantUnbans int
		}{
			{name: "ban rejected", fail: "banChatMember", wantBans: 1, wantUnbans: 1},
			{name: "unban rejected", fail: "unbanChatMember", wantUnbans: 1},
			{name: "crash after ban succeeded", crash: true, wantUnbans: 1},
			{name: "new manual ban before unban", fail: "unbanChatMember", newer: true},
		} {
			t.Run(reason+"/"+scenario.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "punishment.db")
				b, fc := replyAnchorBot(t, path)
				ctx := b.runCtx
				serviceableChat(t, b, b.db, testChatID)
				if err := b.db.SetWelcomeMode(ctx, testChatID, string(welcomeReply)); err != nil {
					t.Fatal(err)
				}
				run := expirePunishmentCheck(t, b, reason)
				old := kickUnbanBackoffs
				kickUnbanBackoffs = []time.Duration{0, time.Millisecond}
				t.Cleanup(func() { kickUnbanBackoffs = old })
				if scenario.crash {
					interruptAfterKickBan(t, b, fc, run)
				} else {
					fc.err[scenario.fail] = &telegoapi.Error{ErrorCode: 403, Description: "no rights"}
					run()
				}
				p, ok, err := b.db.GetPunishment(ctx, testChatID, testUserID)
				if err != nil || !ok || p.Attempt != 1 || p.Kind != storage.EventKick ||
					p.BanConfirmed != (scenario.fail == "unbanChatMember") {
					t.Fatalf("durable phase: %+v, %v, %v", p, ok, err)
				}
				if k := statsKinds(t, b.db, testChatID, testUserID); k[storage.EventKick] != 0 {
					t.Fatalf("kick recorded before completion: %v", k)
				}
				if err := b.db.Close(); err != nil {
					t.Fatal(err)
				}

				restarted, recovered := replyAnchorBot(t, path)
				ctx = restarted.runCtx
				if scenario.wantBans == 0 {
					until := p.BanUntil
					if scenario.newer {
						until = 0
					}
					recovered.resp["getChatMember"] = fmt.Sprintf(
						`{"status":"kicked","until_date":%d,"user":{"id":7}}`, until)
				}
				if n, err := restarted.restorePending(ctx); err != nil || n != 0 {
					t.Fatalf("old captcha armed instead of using its decision: %d, %v", n, err)
				}
				if n, err := restarted.restorePendingReplies(ctx); err != nil || n != 0 {
					t.Fatalf("old reply armed instead of using its decision: %d, %v", n, err)
				}
				restarted.reconcilePunishments(ctx)
				restarted.reconcilePunishments(ctx)
				if recovered.callCount("banChatMember") != scenario.wantBans ||
					recovered.callCount("unbanChatMember") != scenario.wantUnbans {
					t.Fatalf("wrong phase replay: %v", recovered.callList())
				}
				if scenario.wantBans > 0 && !strings.Contains(recovered.callBodies("banChatMember")[0],
					fmt.Sprintf(`"until_date":%d`, p.BanUntil)) {
					t.Fatalf("retry changed the temporary ban's identity: %v", recovered.callList())
				}
				if count, err := restarted.db.AttemptCount(ctx, testChatID, testUserID, attemptsTTL); err != nil || count != 1 {
					t.Fatalf("recovery counted one failure as %d attempts, err=%v", count, err)
				}
				if rows, err := restarted.db.LoadPunishments(ctx); err != nil || len(rows) != 0 {
					t.Fatalf("decision did not settle: %+v, %v", rows, err)
				}
				if k := statsKinds(t, restarted.db, testChatID, testUserID); k[storage.EventKick] != scenario.wantUnbans ||
					k[storage.EventBan] != 0 {
					t.Fatalf("unexpected terminal event: %v", k)
				}
			})
		}
	}
}

func TestManualKickWithGlobalSpamFlag(t *testing.T) {
	for _, scenario := range []string{"success", "status unavailable", "already banned", "recover temporary ban", "preserve newer ban"} {
		t.Run(scenario, func(t *testing.T) {
			shortAdmissionRetries(t)
			old := kickUnbanBackoffs
			kickUnbanBackoffs = []time.Duration{0}
			t.Cleanup(func() { kickUnbanBackoffs = old })
			path := filepath.Join(t.TempDir(), "kick.db")
			b, fc := replyAnchorBot(t, path)
			ctx := b.runCtx
			serviceableChat(t, b, b.db, testChatID)
			if err := b.db.AddSpamBanned(ctx, testUserID, -100500, time.Now()); err != nil {
				t.Fatal(err)
			}
			banned := func(until int64) string {
				return fmt.Sprintf(`{"status":"kicked","until_date":%d,"user":{"id":%d}}`, until, testUserID)
			}
			if scenario == "already banned" {
				fc.respSeq["getChatMember"] = []string{defaultChatMember(&telegoapi.RequestData{BodyRaw: []byte(fmt.Sprintf(`{"user_id":%d}`, testBotID))}), banned(0)}
			}
			if scenario == "status unavailable" {
				fc.errWhen = func(method string, data *telegoapi.RequestData) bool {
					return method == "getChatMember" && strings.Contains(string(data.BodyRaw), fmt.Sprintf(`"user_id":%d`, testUserID))
				}
			}
			recovering := scenario == "recover temporary ban" || scenario == "preserve newer ban"
			if recovering {
				fc.err["unbanChatMember"] = &telegoapi.Error{ErrorCode: 502}
			}
			msg := telego.Message{MessageID: 99, Text: "/kick", Chat: telego.Chat{ID: testChatID, Type: telego.ChatTypeSupergroup},
				From: &telego.User{ID: 200}, ReplyToMessage: &telego.Message{MessageID: 98, From: &telego.User{ID: testUserID}}}
			if err := b.handleKickCommand(nil, msg); err != nil {
				t.Fatal(err)
			}
			text := strings.Join(fc.callBodies("sendMessage"), "")
			p, pending, err := b.db.GetPunishment(ctx, testChatID, testUserID)
			if err != nil || pending != (recovering || scenario == "status unavailable") {
				t.Fatalf("wrong durable result: %+v / %t / %v", p, pending, err)
			}
			wantBan, wantUnban, wantEvent := 0, 0, 0
			switch {
			case scenario == "success":
				wantBan, wantUnban, wantEvent = 1, 1, 1
				if !strings.Contains(text, "кикнут") {
					t.Fatalf("kick success missing from response: %s", text)
				}
			case recovering:
				wantBan, wantUnban = 1, 1
			case scenario == "already banned":
				if strings.Contains(text, "сверит заявку") || strings.Contains(text, "кикнут") {
					t.Fatalf("a dropped kick should not be reported as pending or complete: %s", text)
				}
			}
			if pending && !strings.Contains(text, "сверит заявку") {
				t.Fatalf("pending kick status missing from response: %s", text)
			}
			if fc.callCount("banChatMember") != wantBan || fc.callCount("unbanChatMember") != wantUnban {
				t.Fatalf("wrong kick phases: %v", fc.callList())
			}
			if recovering {
				if err := b.db.Close(); err != nil {
					t.Fatal(err)
				}
				b, fc = replyAnchorBot(t, path)
				ctx = b.runCtx
				until := int64(0)
				if scenario == "recover temporary ban" {
					until, wantEvent = p.BanUntil, 1
				}
				fc.resp["getChatMember"] = banned(until)
				b.reconcilePunishments(ctx)
				b.reconcilePunishments(ctx)
				if fc.callCount("banChatMember") != 0 || fc.callCount("unbanChatMember") != wantEvent {
					t.Fatalf("restart touched a foreign ban or repeated kick work: %v", fc.callList())
				}
			}
			if s, err := b.db.QueryStats(ctx, testChatID, time.Now().Add(-time.Minute), time.Now().Add(time.Minute)); err != nil || s.ModKicked != wantEvent {
				t.Fatalf("wrong manual kick event count: %d / %v", s.ModKicked, err)
			}
			if marked, err := b.db.IsSpamBanned(ctx, testUserID); err != nil || !marked {
				t.Fatalf("manual kick changed the global flag: %t / %v", marked, err)
			}
		})
	}
}

func expirePunishmentCheck(t *testing.T, b *Bot, reason string) func() {
	t.Helper()
	ctx := b.runCtx
	expired := time.Now().Add(-time.Second)
	if reason == storage.ReasonCaptcha {
		p := b.store.Put(testChatID, testUserID, 77, 0, expired, 0, 0, captchaStages)
		if err := b.db.PutPending(ctx, storage.PendingRow{ChatID: testChatID, UserID: testUserID,
			MessageID: p.MessageID, Stage: p.Stage, ExpiresAt: expired}); err != nil {
			t.Fatal(err)
		}
		return func() { b.advanceCaptchaStage(ctx, testChatID, testUserID, p) }
	}
	p := b.replies.Put(testChatID, testUserID, expired, 0, captchaStages, 0)
	if err := b.db.PutPendingReply(ctx, storage.PendingReply{ChatID: testChatID, UserID: testUserID,
		Stage: p.Stage, ExpiresAt: expired}); err != nil {
		t.Fatal(err)
	}
	return func() { b.advanceReplyWait(ctx, testChatID, testUserID, p) }
}

func TestFullUnbanCancelsOldPunishmentsAfterRestart(t *testing.T) {
	old := kickUnbanBackoffs
	kickUnbanBackoffs = []time.Duration{0}
	t.Cleanup(func() { kickUnbanBackoffs = old })
	path := filepath.Join(t.TempDir(), "punishment.db")
	b, fc := replyAnchorBot(t, path)
	const other = -100200
	chats := []int64{testChatID, other}
	fc.err["banChatMember"] = &telegoapi.Error{ErrorCode: 502}
	for _, chatID := range chats {
		serviceableChat(t, b, b.db, chatID)
		p, _, err := b.beginPunishment(b.runCtx, chatID, testUserID, storage.ReasonCaptcha, "", 1)
		if err == nil || p.Kind != storage.EventBan || p.Attempt != 1 {
			t.Fatalf("expected a pending permanent captcha ban in chat %d: %+v / %v", chatID, p, err)
		}
	}
	msg := telego.Message{MessageID: 99, Text: "/fullunban",
		Chat: telego.Chat{ID: testChatID, Type: telego.ChatTypeSupergroup}, From: &telego.User{ID: 200},
		ReplyToMessage: &telego.Message{MessageID: 98, From: &telego.User{ID: testUserID}}}
	if err := b.handleFullunbanCommand(nil, msg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return fc.callCount("sendMessage") >= 2 })
	if fc.callCount("unbanChatMember") != 2 || !strings.Contains(strings.Join(fc.callBodies("sendMessage"), ""), "попытки сброшены") {
		t.Fatalf("full unban did not finish in both chats: %v / %v", fc.callList(), fc.callBodies("sendMessage"))
	}
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, recovered := replyAnchorBot(t, path)
	ctx := restarted.runCtx
	restarted.reconcilePunishments(ctx)
	restarted.reconcilePunishments(ctx)
	if recovered.callCount("banChatMember") != 0 || recovered.callCount("unbanChatMember") != 0 {
		t.Fatalf("old punishment replayed after confirmed full unban: %v", recovered.callList())
	}
	for _, chatID := range chats {
		if n, err := restarted.db.AttemptCount(ctx, chatID, testUserID, attemptsTTL); err != nil || n != 0 {
			t.Fatalf("attempts after full unban in chat %d = %d / %v", chatID, n, err)
		}
		p, kind, err := restarted.beginPunishment(ctx, chatID, testUserID, storage.ReasonCaptcha, "new violation", 3)
		if err != nil || p.Attempt != 1 || kind != storage.EventKick {
			t.Fatalf("new violation in chat %d was not handled with a fresh counter: %+v / %s / %v", chatID, p, kind, err)
		}
	}
	if recovered.callCount("banChatMember") != 2 || recovered.callCount("unbanChatMember") != 2 {
		t.Fatalf("only the new violations should produce kick phases: %v", recovered.callList())
	}
}

func TestFullUnbanResumesAfterConfirmationWriteIsInterrupted(t *testing.T) {
	old := kickUnbanBackoffs
	kickUnbanBackoffs = []time.Duration{0}
	t.Cleanup(func() { kickUnbanBackoffs = old })
	for _, origin := range []bool{true, false} {
		t.Run(fmt.Sprintf("origin=%t", origin), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "punishment.db")
			b, fc := replyAnchorBot(t, path)
			serviceableChat(t, b, b.db, testChatID)
			fc.err["banChatMember"] = &telegoapi.Error{ErrorCode: 502}
			if _, _, err := b.beginPunishment(b.runCtx, testChatID, testUserID, storage.ReasonCaptcha, "", 1); err == nil {
				t.Fatal("expected an old pending punishment")
			}
			ctx, stop := context.WithCancel(b.runCtx)
			defer stop()
			fc.errWhen = func(method string, _ *telegoapi.RequestData) bool {
				if method == "unbanChatMember" {
					stop()
				}
				return false
			}
			text, err := b.execFullUnban(ctx, testChatID, testUserID, origin, &fullUnbanRequest{})
			if err != nil || !strings.Contains(text, "Разбан подтверждён") || !strings.Contains(text, "сброс попыток ещё не завершены") {
				t.Fatalf("partial result was not reported: %q / %v", text, err)
			}
			if err := b.db.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, recovered := replyAnchorBot(t, path)
			ctx = restarted.runCtx
			for range 2 {
				restarted.reconcilePunishments(ctx)
				restarted.reconcileAdmissions(ctx)
			}
			if recovered.callCount("banChatMember") != 0 || recovered.callCount("unbanChatMember") != 1 {
				t.Fatalf("restart should only reconcile the new unban: %v", recovered.callList())
			}
			if n, err := restarted.db.AttemptCount(ctx, testChatID, testUserID, attemptsTTL); err != nil || n != 0 {
				t.Fatalf("attempt reset did not survive restart: %d / %v", n, err)
			}
		})
	}
}

func TestFullUnbanFanoutPreservesActiveCaptchaWithoutPunishment(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	putCaptcha(b, db, testChatID, testUserID, 77)
	if text := b.fullUnbanChat(testChatID, testUserID, 0); !strings.Contains(text, "попытки сброшены") {
		t.Fatalf("full unban failed: %q", text)
	}
	if _, ok := b.store.Get(testChatID, testUserID); !ok || len(pendingRows(t, db)) != 1 || fc.callCount("restrictChatMember") != 0 {
		t.Fatalf("cross-chat unban interfered with the active captcha: %v", fc.callList())
	}
}

type afterKickCaller struct {
	*fakeCaller
	stop *int
}

func (c afterKickCaller) Call(ctx context.Context, url string, data *telegoapi.RequestData) (*telegoapi.Response, error) {
	r, err := c.fakeCaller.Call(ctx, url, data)
	if strings.HasSuffix(url, "/banChatMember") && err == nil {
		panic(c.stop)
	}
	return r, err
}

func interruptAfterKickBan(t *testing.T, b *Bot, fc *fakeCaller, run func()) {
	t.Helper()
	stop := new(int)
	api, err := telego.NewBot("123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi",
		telego.WithAPICaller(afterKickCaller{fakeCaller: fc, stop: stop}), telego.WithDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	b.api = api
	defer func() {
		if recovered := recover(); recovered != stop {
			t.Errorf("want stop after successful ban, got %v", recovered)
		}
	}()
	run()
}
