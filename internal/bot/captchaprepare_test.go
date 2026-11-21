package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

type interruptedCaptchaCaller struct {
	*fakeCaller
	before func(string, *telegoapi.RequestData)
}

func (c interruptedCaptchaCaller) Call(ctx context.Context, url string, data *telegoapi.RequestData) (*telegoapi.Response, error) {
	c.before(url, data)
	return c.fakeCaller.Call(ctx, url, data)
}

func TestCaptchaPreparationCrashRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, stopAt string
		restricted   bool
	}{
		{"before restriction request", "restrictChatMember", false},
		{"after restriction before sending", "sendMessage", true},
		{"after sending before saving ID", "getChatMember", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "captcha.db")
			b, _, fc := newFlowBot(t)
			b.db = openPendingRestoreDB(t, ctx, path)
			serviceableChat(t, b, b.db, testChatID)
			b.cfg.CaptchaStageInterval = 10 * time.Minute
			var restriction int64
			stop := new(int)
			caller := interruptedCaptchaCaller{fakeCaller: fc, before: func(url string, data *telegoapi.RequestData) {
				method := url[strings.LastIndexByte(url, '/')+1:]
				if method == "restrictChatMember" {
					var req struct {
						UntilDate int64 `json:"until_date"`
					}
					if err := json.Unmarshal(data.BodyRaw, &req); err != nil {
						t.Fatal(err)
					}
					restriction = req.UntilDate
				}
				if method == tc.stopAt {
					panic(stop)
				}
			}}
			api, err := telego.NewBot("123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi", telego.WithAPICaller(caller), telego.WithDiscardLogger())
			if err != nil {
				t.Fatal(err)
			}
			b.api = api
			func() {
				defer func() {
					if crashed := recover(); crashed != stop {
						t.Errorf("want interruption at %s, got %v", tc.stopAt, crashed)
					}
				}()
				b.runCaptcha(testChatID, telego.User{ID: testUserID}, 77)
			}()
			wantSends := 0
			if tc.stopAt == "getChatMember" {
				wantSends = 1
			}
			if got := fc.callCount("sendMessage"); got != wantSends {
				t.Fatalf("sent %d keyboards before crash, want %d", got, wantSends)
			}
			rows := pendingRows(t, b.db)
			if len(rows) != 1 || !rows[0].Preparing() || rows[0].RestrictUntil != restriction || rows[0].ThreadID != 77 {
				t.Fatalf("preparation was not committed with the planned request: %+v, until_date=%d", rows, restriction)
			}
			if d := time.Until(time.Unix(restriction, 0)); d < 30*time.Minute || d > 33*time.Minute {
				t.Fatalf("wrong finite restriction duration: %v", d)
			}
			if err := b.db.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, _, recoveredCalls := newFlowBot(t)
			restarted.db = openPendingRestoreDB(t, ctx, path)
			recoveredCalls.resp["getChat"] = `{"permissions":{"can_send_messages":true,"can_send_polls":false}}`
			if tc.restricted {
				recoveredCalls.resp["getChatMember"] = fmt.Sprintf(
					`{"status":"restricted","user":{"id":7},"is_member":true,"until_date":%d}`, restriction)
			}
			for range 2 {
				if n, err := restarted.restorePending(ctx); n != 0 || err != nil {
					t.Fatalf("restore = %d, %v, want cleanup without timer", n, err)
				}
			}
			if len(pendingRows(t, restarted.db)) != 0 || restarted.store.IsCaptchaActive(testChatID, testUserID) {
				t.Fatal("interrupted preparation became a live captcha")
			}
			wantReleases := 0
			if tc.restricted {
				wantReleases = 1
			}
			if releases := recoveredCalls.callCount("restrictChatMember"); releases != wantReleases {
				t.Fatalf("releases = %d, want %d: %v", releases, wantReleases, recoveredCalls.callList())
			}
			if wantReleases == 1 && !strings.Contains(recoveredCalls.callBodies("restrictChatMember")[0], `"can_send_polls":false`) {
				t.Fatal("release did not preserve the chat's default permissions")
			}
			if k := statsKinds(t, restarted.db, testChatID, testUserID); k[storage.EventAbort] != 1 ||
				k[storage.EventKick]+k[storage.EventBan] != 0 || recoveredCalls.callCount("sendMessage") != 0 {
				t.Fatalf("unexpected recovery effect: stats=%v, calls=%v", k, recoveredCalls.callList())
			}
		})
	}
}

func TestCaptchaPreparationRecoveryKeepsOtherRestriction(t *testing.T) {
	b, db, fc := newFlowBot(t)
	ctx := context.Background()
	serviceableChat(t, b, db, testChatID)
	row, err := b.newCaptchaPreparation(ctx, testChatID, testUserID, 0, b.chatSettings(ctx, testChatID))
	if err != nil {
		t.Fatal(err)
	}
	fc.resp["getChatMember"] = fmt.Sprintf(`{"status":"restricted","user":{"id":7},"is_member":true,"until_date":%d}`, row.RestrictUntil+120)
	if n, err := b.restorePending(ctx); n != 0 || err != nil {
		t.Fatalf("restore = %d, %v", n, err)
	}
	if fc.callCount("restrictChatMember") != 0 || len(pendingRows(t, db)) != 0 {
		t.Fatalf("restore changed another restriction or retained preparation: %v", fc.callList())
	}
}

func TestLegacyPendingCaptchaContinuesAfterSchemaUpgrade(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "captcha.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
		CREATE TABLE pending_captchas (
			chat_id INTEGER NOT NULL, user_id INTEGER NOT NULL, message_id INTEGER NOT NULL,
			correct_idx INTEGER NOT NULL, expires_at INTEGER NOT NULL, thread_id INTEGER NOT NULL DEFAULT 0,
			ephemeral_msg_id INTEGER NOT NULL DEFAULT 0, stage INTEGER NOT NULL DEFAULT 1,
			suspended INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(chat_id,user_id)
		);
		INSERT INTO pending_captchas VALUES (?, ?, 0, 2, ?, 77, 501, 2, 0);
	`, testChatID, testUserID, time.Now().Add(time.Hour).Unix()); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, fc := newFlowBot(t)
	b.runCtx = ctx
	b.db = openPendingRestoreDB(t, ctx, path)
	serviceableChat(t, b, b.db, testChatID)
	if n, err := b.restorePending(ctx); n != 1 || err != nil {
		t.Fatalf("legacy restore = %d, %v, want one live captcha", n, err)
	}
	p, ok := b.store.Get(testChatID, testUserID)
	if !ok || p.EphemeralID != 501 || p.ThreadID != 77 || p.Stage != 2 || p.CorrectIdx != 2 {
		t.Fatalf("legacy keyboard lost identity: %+v", p)
	}
	if calls := fc.callList(); len(calls) != 1 || calls[0] != "getChatMember" {
		t.Fatalf("legacy activation should only verify membership: %v", calls)
	}
}
