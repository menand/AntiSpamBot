package bot

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestSpamVotingRequiresReadableSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.db")
	b, fc := replyAnchorBot(t, path)
	ctx := b.runCtx
	serviceableChat(t, b, b.db, testChatID)
	vote := createVerdictVote(t, b)
	const voter = int64(901)
	for range 6 {
		if _, err := b.db.RecordMessage(ctx, testChatID, voter, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	whitelist := 8
	if err := b.db.SetSpamWhitelistMsgs(ctx, testChatID, &whitelist); err != nil {
		t.Fatal(err)
	}
	restore := makeChatSettingsUnavailable(t, path)
	if total, err := b.db.UserMessageTotal(ctx, testChatID, voter); err != nil || total != 6 {
		t.Fatalf("message counter must remain available: %d / %v", total, err)
	}
	const reportTarget = int64(777)
	runReport := func() {
		t.Helper()
		if err := b.handleSpamCommand(nil, reportCommand(voter, &telego.User{ID: reportTarget, FirstName: "Test"})); err != nil {
			t.Fatal(err)
		}
	}
	runReport()
	pressSpamVote(t, b, int64(vote.BotMsgID), voter)
	if !strings.Contains(strings.Join(fc.callBodies("sendMessage"), ""), "Не удалось прочитать настройки репорта") ||
		!strings.Contains(strings.Join(fc.callBodies("answerCallbackQuery"), ""), "Не удалось прочитать настройки голосования") {
		t.Fatalf("settings errors did not produce retry feedback: %v", fc.callList())
	}
	assertGate := func(allowed bool) {
		t.Helper()
		hasReport, err := b.db.HasPendingVoteForAuthor(ctx, testChatID, reportTarget)
		if err != nil || hasReport != allowed {
			t.Fatalf("report pending=%v, want %v; %v", hasReport, allowed, err)
		}
		wantBallots := 1
		if allowed {
			wantBallots++
		}
		if yes, no, err := b.db.CountBallots(ctx, testChatID, vote.BotMsgID); err != nil || yes != wantBallots || no != 0 {
			t.Fatalf("vote tally=%d:%d, want %d:0; %v", yes, no, wantBallots, err)
		}
	}
	assertGate(false)

	restore()
	runReport()
	pressSpamVote(t, b, int64(vote.BotMsgID), voter)
	assertGate(false)
	for range 3 {
		if _, err := b.db.RecordMessage(ctx, testChatID, voter, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	runReport()
	pressSpamVote(t, b, int64(vote.BotMsgID), voter)
	assertGate(true)
}

func TestSpamVoteRecoveryWaitsForReadableMargin(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "startup"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.db")
			b, fc := replyAnchorBot(t, path)
			ctx := b.runCtx
			serviceableChat(t, b, b.db, testChatID)
			created := time.Now()
			if expired {
				created = created.Add(-spamVoteTTL - time.Hour)
			}
			const msg = 120
			if err := b.db.PutSpamVote(ctx, storage.SpamVote{
				ChatID: testChatID, BotMsgID: msg, AuthorID: testUserID, CreatedAt: created,
			}); err != nil {
				t.Fatal(err)
			}
			for voter := int64(501); voter < 504; voter++ {
				if _, err := b.db.UpsertBallot(ctx, testChatID, msg, voter, true); err != nil {
					t.Fatal(err)
				}
			}
			margin := 4
			if err := b.db.SetSpamVoteMargin(ctx, testChatID, &margin); err != nil {
				t.Fatal(err)
			}
			restore := makeChatSettingsUnavailable(t, path)
			recoverVotes := b.reconcileSpamVotes
			if expired {
				recoverVotes = b.sweepExpiredVotes
			}
			recoverVotes(ctx)
			if _, found, err := b.db.GetSpamVote(ctx, testChatID, msg); err != nil || !found {
				t.Fatalf("settings error silently closed the vote: %v / %v", found, err)
			}
			restore()
			recoverVotes(ctx)
			if fc.callCount("banChatMember") != 0 {
				t.Fatalf("default margin was incorrectly applied: %v", fc.callList())
			}
			if _, found, err := b.db.GetSpamVote(ctx, testChatID, msg); err != nil || found == expired {
				t.Fatalf("vote presence=%v after valid below-margin result (expired=%v): %v", found, expired, err)
			}
		})
	}
}

func makeChatSettingsUnavailable(t *testing.T, path string) func() {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec("ALTER TABLE chat_settings RENAME TO unavailable_chat_settings"); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if _, err := raw.Exec("ALTER TABLE unavailable_chat_settings RENAME TO chat_settings"); err != nil {
			t.Fatal(err)
		}
	}
}
