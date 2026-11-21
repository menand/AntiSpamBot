package bot

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestAcceptedSpamVerdictSurvivesRestart(t *testing.T) {
	for _, stopAfter := range []string{"decision", "telegram accepted ban"} {
		t.Run(stopAfter, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "verdict.db")
			b, fc := replyAnchorBot(t, path)
			serviceableChat(t, b, b.db, testChatID)
			vote := createVerdictVote(t, b)
			id, err := b.db.AcceptSpamVerdict(b.runCtx, vote.ChatID, vote.BotMsgID, func(yes, no int) (bool, string, bool) {
				return true, fmt.Sprintf("tally %d:%d", yes, no), yes == 1 && no == 0
			})
			if err != nil || id == 0 {
				t.Fatalf("accept did not freeze the vote: %d / %v", id, err)
			}
			if accepted, err := b.db.UpsertBallot(b.runCtx, vote.ChatID, vote.BotMsgID, 501, false); err != nil || accepted {
				t.Fatalf("accepted verdict was changed by a later ballot: %v / %v", accepted, err)
			}
			if accepted, err := b.db.AcceptSpamVerdict(b.runCtx, vote.ChatID, vote.BotMsgID, func(int, int) (bool, string, bool) {
				return false, "later golden vote", true
			}); err != nil || accepted != 0 {
				t.Fatalf("accepted verdict was changed by a later admin: %v / %v", accepted, err)
			}
			if stopAfter == "telegram accepted ban" {
				ctx, stop := context.WithCancel(b.runCtx)
				defer stop()
				fc.errWhen = func(method string, _ *telegoapi.RequestData) bool {
					if method == "banChatMember" {
						stop()
					}
					return false
				}
				b.applySpamVerdict(ctx, id, vote.ChatID, vote.AuthorID)
				if fc.callCount("banChatMember") != 1 {
					t.Fatal("failed to reach the local ban before stopping")
				}
			}
			if global, err := b.db.IsSpamBanned(b.runCtx, vote.AuthorID); err != nil || global {
				t.Fatalf("global state written before confirming the result: %v / %v", global, err)
			}
			if err := b.db.Close(); err != nil {
				t.Fatal(err)
			}

			restarted, restartCaller := replyAnchorBot(t, path)
			restarted.reconcileSpamVotes(restarted.runCtx)
			restarted.resumeSpamVerdicts(restarted.runCtx)
			restarted.sweepExpiredVotes(restarted.runCtx)
			if restartCaller.callCount("banChatMember") != 1 {
				t.Fatalf("restart did not resolve the saved verdict exactly once: %v", restartCaller.callList())
			}
			if global, err := restarted.db.IsSpamBanned(restarted.runCtx, vote.AuthorID); err != nil || !global {
				t.Fatalf("missing global entry after a confirmed local ban: %v / %v", global, err)
			}
			kinds := statsKinds(t, restarted.db, vote.ChatID, vote.AuthorID)
			if kinds[storage.EventSpamBan] != 1 {
				t.Fatalf("restart must record one event: %v", kinds)
			}
			if pending, err := restarted.db.ListSpamVerdicts(restarted.runCtx); err != nil || len(pending) != 0 {
				t.Fatalf("finished verdicts remained pending: %v / %v", pending, err)
			}
		})
	}
}

func TestAcceptedSpamVerdictEndsOnPermissionFailure(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	vote := createVerdictVote(t, b)
	fc.err["banChatMember"] = &telegoapi.Error{ErrorCode: 403, Description: "forbidden"}
	b.acceptTalliedSpamVerdict(vote, 1)
	attempts := fc.callCount("banChatMember")
	b.reconcileSpamVotes(b.runCtx)
	b.resumeSpamVerdicts(b.runCtx)
	if attempts == 0 || fc.callCount("banChatMember") != attempts {
		t.Fatalf("permission error should end the verdict: %v", fc.callList())
	}
	if global, err := db.IsSpamBanned(b.runCtx, vote.AuthorID); err != nil || global {
		t.Fatalf("permission error affected global state: %v / %v", global, err)
	}
	if kinds := statsKinds(t, db, vote.ChatID, vote.AuthorID); kinds[storage.EventSpamBan] != 0 {
		t.Fatalf("permission error recorded a ban: %v", kinds)
	}
	if pending, err := db.ListSpamVerdicts(b.runCtx); err != nil || len(pending) != 0 {
		t.Fatalf("definitively rejected verdicts remained pending: %v / %v", pending, err)
	}
	if got := strings.Join(fc.callBodies("editMessageText"), ""); !strings.Contains(got, "Забанить пользователя не удалось") {
		t.Fatalf("missing visible terminal result: %s", got)
	}
}

func TestPardonCancelsAcceptedSpamVerdictsAfterRestart(t *testing.T) {
	for _, action := range []string{"u", "f"} {
		for _, tryBan := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/temporary-error=%t", action, tryBan), func(t *testing.T) {
				shortAdmissionRetries(t)
				path := filepath.Join(t.TempDir(), "spam.db")
				b, fc := replyAnchorBot(t, path)
				chats := []int64{testChatID}
				if action == "f" {
					chats = append(chats, -100200)
				}
				fc.err["banChatMember"] = &telegoapi.Error{ErrorCode: 502}
				for _, chatID := range chats {
					serviceableChat(t, b, b.db, chatID)
					v := acceptTestSpamVerdict(t, b, chatID)
					if tryBan {
						b.applySpamVerdict(b.runCtx, v.ID, v.ChatID, v.AuthorID)
					}
				}
				var full fullUnbanRequest
				if _, err := b.execUnmod(action, testChatID, testUserID, &full); err != nil {
					t.Fatal(err)
				}
				if action == "f" {
					b.fullUnbanEverywhere(testUserID, 0, full)
				}
				if got := fc.callCount("unbanChatMember"); got != len(chats) {
					t.Fatalf("pardon did not apply in all intended chats: %v", fc.callList())
				}
				if err := b.db.Close(); err != nil {
					t.Fatal(err)
				}
				restarted, recovered := replyAnchorBot(t, path)
				for range 2 {
					restarted.resumeSpamVerdicts(restarted.runCtx)
				}
				if recovered.callCount("banChatMember") != 0 {
					t.Fatalf("old spam verdict banned a pardoned user: %v", recovered.callList())
				}
				if banned, err := restarted.db.IsSpamBanned(restarted.runCtx, testUserID); err != nil || banned {
					t.Fatalf("old spam verdict restored the global flag: %t / %v", banned, err)
				}
				for _, chatID := range chats {
					if got := statsKinds(t, restarted.db, chatID, testUserID)[storage.EventSpamBan]; got != 0 {
						t.Fatalf("old spam verdict created %d events in chat %d", got, chatID)
					}
				}
				if pending, err := restarted.db.ListSpamVerdicts(restarted.runCtx); err != nil || len(pending) != 0 {
					t.Fatalf("pardoned verdicts remained pending: %v / %v", pending, err)
				}
			})
		}
	}
}

func TestDelayedFullUnbanKeepsNewSpamVerdict(t *testing.T) {
	b, db, fc := newFlowBot(t)
	const other = -100200
	serviceableChat(t, b, db, testChatID)
	serviceableChat(t, b, db, other)
	var full fullUnbanRequest
	if _, err := b.execUnmod("f", testChatID, testUserID, &full); err != nil {
		t.Fatal(err)
	}
	v := acceptTestSpamVerdict(t, b, other)
	if saved, err := db.RecordSpamOutcome(b.runCtx, v, storage.SpamBanApplied, time.Now(), "test"); err != nil || !saved {
		t.Fatalf("record fresh verdict: %t / %v", saved, err)
	}
	if err := db.InitSpamFanout(b.runCtx, v.ID, nil); err != nil {
		t.Fatal(err)
	}
	if finished, err := db.FinishSpamVerdict(b.runCtx, v.ID, v.ChatID); err != nil || !finished {
		t.Fatalf("finish fresh verdict: %t / %v", finished, err)
	}
	b.fullUnbanEverywhere(testUserID, 0, full)
	if fc.callCount("unbanChatMember") != 1 {
		t.Fatalf("delayed full unban removed a fresh ban: %v", fc.callList())
	}
	if banned, err := db.IsSpamBanned(b.runCtx, testUserID); err != nil || !banned {
		t.Fatalf("fresh global spam flag lost: %t / %v", banned, err)
	}
}

func TestLocalPardonPreservesIndependentVerdict(t *testing.T) {
	b, db, fc := newFlowBot(t)
	const other = -100200
	serviceableChat(t, b, db, testChatID)
	serviceableChat(t, b, db, other)
	v := acceptTestSpamVerdict(t, b, other)
	if _, err := b.execUnmod("u", testChatID, testUserID, nil); err != nil {
		t.Fatal(err)
	}
	b.resumeSpamVerdicts(b.runCtx)
	if got := statsKinds(t, db, v.ChatID, testUserID)[storage.EventSpamBan]; got != 1 {
		t.Fatalf("independent verdict must produce one local event, got %d", got)
	}
	if banned, err := db.IsSpamBanned(b.runCtx, testUserID); err != nil || !banned {
		t.Fatalf("independent verdict lost its global flag: %t / %v", banned, err)
	}
	if banned, err := db.IsSpamBannedInChat(b.runCtx, testChatID, testUserID); err != nil || banned {
		t.Fatalf("pardon must also protect reentry in its chat: %t / %v", banned, err)
	}
	if fc.callCount("banChatMember") != 1 {
		t.Fatalf("independent verdict must reach its own chat and respect local pardon elsewhere: %v", fc.callList())
	}
}

func TestPardonStopsCrossBanWaitingForRights(t *testing.T) {
	b, db, fc := newFlowBot(t)
	const other = -100200
	serviceableChat(t, b, db, testChatID)
	serviceableChat(t, b, db, other)
	v := seedConfirmedCrossBan(t, b, testChatID, other)
	unlock, err := b.rights.lock(b.runCtx, other, testUserID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := b.banEverywhere(b.runCtx, v)
		done <- err
	}()
	waitFor(t, func() bool {
		b.rights.mu.Lock()
		defer b.rights.mu.Unlock()
		return len(b.rights.lanes[chatUser{other, testUserID}]) == 2
	})
	if _, err := b.execUnmod("u", testChatID, testUserID, nil); err != nil {
		t.Fatal(err)
	}
	unlock()
	unlock = nil
	select {
	case err := <-done:
		if err != nil || fc.callCount("banChatMember") != 0 {
			t.Fatalf("pardoned verdict sent an old cross-ban: %v / %v", err, fc.callList())
		}
	case <-time.After(time.Second):
		t.Fatal("cross-ban did not finish after the rights queue was released")
	}
	if got := statsKinds(t, db, testChatID, testUserID)[storage.EventSpamBan]; got != 1 {
		t.Fatalf("pardon rewrote the already confirmed historic event: %d", got)
	}
	if banned, err := db.IsSpamBanned(b.runCtx, testUserID); err != nil || banned {
		t.Fatalf("pardoned verdict restored the global flag: %t / %v", banned, err)
	}
}

func seedConfirmedCrossBan(t *testing.T, b *Bot, source int64, targets ...int64) storage.SpamVerdict {
	t.Helper()
	v := acceptTestSpamVerdict(t, b, source)
	if saved, err := b.db.RecordSpamOutcome(b.runCtx, v, storage.SpamBanApplied, time.Now(), "test"); err != nil || !saved {
		t.Fatalf("record local ban: %t / %v", saved, err)
	}
	if err := b.db.InitSpamFanout(b.runCtx, v.ID, targets); err != nil {
		t.Fatal(err)
	}
	v.Outcome, v.FanoutReady = storage.SpamBanApplied, true
	return v
}

func TestSpamFanoutResumesPerChat(t *testing.T) {
	for _, scenario := range []string{"before first target", "between targets", "after Telegram before saving", "definite refusal", "later full unban"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fanout.db")
			b, fc := replyAnchorBot(t, path)
			ctx := b.runCtx
			const first, second = -100300, -100200
			for _, id := range []int64{testChatID, first, second} {
				serviceableChat(t, b, b.db, id)
			}
			v := acceptTestSpamVerdict(t, b, testChatID)
			v, ready := b.prepareSpamVerdict(ctx, v.ID, v.ChatID, v.AuthorID)
			if !ready || v.Outcome != storage.SpamBanApplied || fc.callCount("banChatMember") != 1 {
				t.Fatalf("local ban did not reach durable success: %+v / %v", v, fc.callList())
			}
			switch scenario {
			case "between targets", "later full unban":
				if err := b.reconcileCrossBan(ctx, v, first); err != nil {
					t.Fatal(err)
				}
			case "after Telegram before saving":
				interrupted, stop := context.WithCancel(ctx)
				defer stop()
				fc.errWhen = func(method string, _ *telegoapi.RequestData) bool {
					if method == "banChatMember" {
						stop()
					}
					return false
				}
				if err := b.reconcileCrossBan(interrupted, v, first); err == nil || fc.callCount("banChatMember") != 2 {
					t.Fatalf("not interrupted after the network request: %v / %v", err, fc.callList())
				}
				if state, found, err := b.db.CrossBanStatus(ctx, v.ID, first); err != nil || !found || state != storage.CrossBanStarted {
					t.Fatalf("uncertain target was lost: %v / %t / %v", state, found, err)
				}
			case "definite refusal":
				fc.err["banChatMember"] = &telegoapi.Error{ErrorCode: 403}
				if err := b.reconcileCrossBan(ctx, v, first); err != nil {
					t.Fatal(err)
				}
				if state, found, err := b.db.CrossBanStatus(ctx, v.ID, first); err != nil || !found || state != storage.CrossBanSkipped || fc.callCount("banChatMember") != 2 {
					t.Fatalf("explicit refusal did not stop after one request: %v / %t / %v / %v", state, found, err, fc.callList())
				}
			}
			if err := b.db.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, recovered := replyAnchorBot(t, path)
			ctx = restarted.runCtx
			if scenario == "after Telegram before saving" {
				recovered.resp["getChatMember"] = `{"status":"kicked","until_date":0,"user":{"id":7}}`
			}
			if scenario == "later full unban" {
				var full fullUnbanRequest
				if _, err := restarted.execUnmod("f", testChatID, testUserID, &full); err != nil {
					t.Fatal(err)
				}
				restarted.fullUnbanEverywhere(testUserID, 0, full)
				if global, err := restarted.db.IsSpamBanned(ctx, testUserID); err != nil || global {
					t.Fatalf("full unban did not clear old global spam mark: %t / %v", global, err)
				}
			}
			for range 2 {
				restarted.resumeSpamVerdicts(ctx)
			}
			wantBans := 1
			switch scenario {
			case "before first target":
				wantBans = 2
			case "later full unban":
				wantBans = 0
			}
			bodies := recovered.callBodies("banChatMember")
			if len(bodies) != wantBans || (wantBans > 0 && !strings.Contains(bodies[len(bodies)-1], `"chat_id":-100200`)) {
				t.Fatalf("restart sent the wrong bans: %v, want %d", bodies, wantBans)
			}
			if scenario == "after Telegram before saving" && recovered.callCount("getChatMember") != 1 {
				t.Fatalf("uncertain target was not checked exactly once: %v", recovered.callList())
			}
			if kinds := statsKinds(t, restarted.db, testChatID, testUserID); kinds[storage.EventSpamBan] != 1 {
				t.Fatalf("restart changed the local spam ban count: %v", kinds)
			}
			if rows, err := restarted.db.ListSpamVerdicts(ctx); err != nil || len(rows) != 0 {
				t.Fatalf("settled cross-ban remained pending: %+v / %v", rows, err)
			}
		})
	}
}

func acceptTestSpamVerdict(t *testing.T, b *Bot, chatID int64) storage.SpamVerdict {
	t.Helper()
	v := storage.SpamVote{ChatID: chatID, AuthorID: testUserID, BotMsgID: 120, TargetMsgID: 110, CreatedAt: time.Now()}
	if ok, err := b.db.PutSpamVoteOnce(b.runCtx, v); err != nil || !ok {
		t.Fatalf("create vote: %t / %v", ok, err)
	}
	id, err := b.db.AcceptSpamVerdict(b.runCtx, chatID, v.BotMsgID, func(int, int) (bool, string, bool) {
		return true, "test", true
	})
	if err != nil || id == 0 {
		t.Fatalf("accept verdict: %d / %v", id, err)
	}
	return storage.SpamVerdict{ID: id, SpamVote: v, IsSpam: true}
}

func createVerdictVote(t *testing.T, b *Bot) storage.SpamVote {
	t.Helper()
	vote := storage.SpamVote{
		ChatID: testChatID, AuthorID: testUserID, BotMsgID: 120, TargetMsgID: 110, CreatedAt: time.Now(),
	}
	if ok, err := b.db.PutSpamVoteOnce(b.runCtx, vote); err != nil || !ok {
		t.Fatalf("create vote: %v / %v", ok, err)
	}
	if ok, err := b.db.UpsertBallot(b.runCtx, vote.ChatID, vote.BotMsgID, 501, true); err != nil || !ok {
		t.Fatalf("create ballot: %v / %v", ok, err)
	}
	return vote
}

func TestDirectSpamVerdictReconcilesAfterTelegramSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "direct.db")
	b, fc := replyAnchorBot(t, path)
	const otherChat = -100200
	serviceableChat(t, b, b.db, testChatID)
	serviceableChat(t, b, b.db, otherChat)
	ctx, cancel := context.WithCancel(b.runCtx)
	defer cancel()
	b.runCtx = ctx
	fc.errWhen = func(method string, _ *telegoapi.RequestData) bool {
		if method == "banChatMember" {
			cancel()
		}
		return false
	}
	v := storage.SpamVote{ChatID: testChatID, BotMsgID: -testUserID, TargetMsgID: 110, AuthorID: testUserID, CreatedAt: time.Now()}
	if banned, id := b.executeSpamBan(v, storage.ReasonVotePrefix, "test"); !banned || id != 0 {
		t.Fatalf("expected Telegram success before the outcome write: %t / %d", banned, id)
	}
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, recovered := replyAnchorBot(t, path)
	recovered.resp["getChatMember"] = `{"status":"kicked","until_date":0,"user":{"id":7}}`
	if pending, err := restarted.db.ListSpamVerdicts(restarted.runCtx); err != nil || len(pending) != 1 || pending[0].Outcome != "" {
		t.Fatalf("direct intent missing after interrupted write: %+v / %v", pending, err)
	}
	for range 2 {
		restarted.resumeSpamVerdicts(restarted.runCtx)
	}
	bodies := recovered.callBodies("banChatMember")
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"chat_id":-100200`) || recovered.callCount("getChatMember") != 1 {
		t.Fatalf("recovery should verify the source and ban only the target: %v / %v", bodies, recovered.callList())
	}
	if count := statsKinds(t, restarted.db, testChatID, testUserID)[storage.EventSpamBan]; count != 1 {
		t.Fatalf("recovered direct ban count = %d, want 1", count)
	}
	if rows, err := restarted.db.ListSpamVerdicts(restarted.runCtx); err != nil || len(rows) != 0 {
		t.Fatalf("settled direct verdict remained pending: %+v / %v", rows, err)
	}
}

func TestDirectSpamVerdictRefusalKeepsTargetDeletionWithoutFanout(t *testing.T) {
	shortAdmissionRetries(t)
	b, fc := replyAnchorBot(t, filepath.Join(t.TempDir(), "denied.db"))
	serviceableChat(t, b, b.db, testChatID)
	serviceableChat(t, b, b.db, -100200)
	fc.err["banChatMember"] = &telegoapi.Error{ErrorCode: 403}
	v := storage.SpamVote{ChatID: testChatID, BotMsgID: -testUserID, TargetMsgID: 110, AuthorID: testUserID, CreatedAt: time.Now()}
	if banned, id := b.executeSpamBan(v, storage.ReasonVotePrefix, "test"); banned || id != 0 {
		t.Fatalf("definite refusal reported success: %t / %d", banned, id)
	}
	b.resumeSpamVerdicts(b.runCtx)
	deletes := fc.callBodies("deleteMessage")
	if fc.callCount("banChatMember") != len(tgBackoffs) || len(deletes) != 1 || !strings.Contains(deletes[0], `"message_id":110`) {
		t.Fatalf("definite refusal must keep fallback deletion without fanout: %v", fc.callList())
	}
	if global, err := b.db.IsSpamBanned(b.runCtx, testUserID); err != nil || global {
		t.Fatalf("definite refusal set a global ban: %t / %v", global, err)
	}
	if count := statsKinds(t, b.db, testChatID, testUserID)[storage.EventSpamBan]; count != 0 {
		t.Fatalf("definite refusal recorded %d ban events", count)
	}
}
