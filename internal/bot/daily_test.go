package bot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

type reportEditErrorCaller struct {
	*fakeCaller
	err error
}

func (c reportEditErrorCaller) Call(ctx context.Context, url string, data *telegoapi.RequestData) (*telegoapi.Response, error) {
	r, err := c.fakeCaller.Call(ctx, url, data)
	if err == nil && strings.HasSuffix(url, "/editMessageText") {
		return nil, c.err
	}
	return r, err
}

func TestDMReportReplacesOnlyDeletedMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"deleted", &telegoapi.Error{ErrorCode: 400, Description: "Bad Request: message to edit not found"}},
		{"other 400", &telegoapi.Error{ErrorCode: 400, Description: "Bad Request: message can't be edited"}},
		{"timeout", context.DeadlineExceeded},
		{"rate limit", &telegoapi.Error{ErrorCode: 429, Description: "Too Many Requests"}},
		{"blocked", &telegoapi.Error{ErrorCode: 403, Description: "Forbidden: bot was blocked by the user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			ctx := b.runCtx
			const user int64 = 701
			for i, title := range []string{"A Former", "B Current", "C Delayed"} {
				id := int64(-10001) - int64(i)
				serviceableChat(t, b, db, id)
				if err := db.RememberChat(ctx, storage.ChatInfo{ChatID: id, Title: title, Type: "supergroup"}); err != nil {
					t.Fatal(err)
				}
				b.cacheAdmin(id, user, true)
			}
			if err := db.SetDailyReport(ctx, user, true); err != nil {
				t.Fatal(err)
			}
			const member = `{"status":"member","user":{"id":701,"first_name":"Test"}}`
			const admin = `{"status":"administrator","user":{"id":701,"first_name":"Test"}}`
			fc.respSeq["getChatMember"] = []string{admin, admin, admin, member, admin, admin, member, admin, admin}
			fc.errSeq["getChatMember"] = []*telegoapi.Error{nil, nil, {ErrorCode: 500}}
			fc.respSeq["sendMessage"] = []string{privateSentJSON(user, 551), privateSentJSON(user, 552)}
			fc.resp["editMessageText"] = privateSentJSON(user, 551)
			now := time.Date(2030, 6, 2, 12, 0, 0, 0, storage.StatsLocation)
			from, until := statsRange(periodYesterday, now)
			day := storage.DayOf(now)
			tick := func() { b.maybeSendDMReports(ctx, now, from, until, day) }
			tick()
			id, partialHash, err := db.DailyReportProgress(ctx, user, day)
			if err != nil || id != 551 || partialHash == "" {
				t.Fatalf("partial report not saved: %d / %q / %v", id, partialHash, err)
			}
			first := b.api
			caller := reportEditErrorCaller{fc, tc.err}
			b.api, err = telego.NewBot("123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi", telego.WithAPICaller(caller), telego.WithDiscardLogger())
			if err != nil {
				t.Fatal(err)
			}
			tick()
			id, savedHash, err := db.DailyReportProgress(ctx, user, day)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "deleted" {
				sends := fc.callBodies("sendMessage")
				if id != 552 || len(sends) != 2 || savedHash == partialHash || strings.Contains(sends[1], "A Former") ||
					!strings.Contains(sends[1], "B Current") || !strings.Contains(sends[1], "C Delayed") {
					t.Fatalf("replacement was not saved with current rights: %d / %q / %v", id, savedHash, sends)
				}
				var replacement struct{ Text string }
				if err := json.Unmarshal([]byte(sends[1]), &replacement); err != nil || savedHash != fmt.Sprintf("%x", sha256.Sum256([]byte(replacement.Text))) {
					t.Fatalf("replacement hash does not match delivered text: %v", err)
				}
				tick()
				if fc.callCount("sendMessage") != 2 || fc.callCount("editMessageText") != 1 {
					t.Fatalf("replacement was duplicated: %v", fc.callList())
				}
				return
			}
			if id != 551 || partialHash != savedHash || fc.callCount("sendMessage") != 1 {
				t.Fatalf("failed edit lost the previous state or sent a replacement: %d / %q / %v", id, savedHash, fc.callList())
			}
			b.api = first
			tick()
			edits := fc.callBodies("editMessageText")
			if tc.name == "blocked" {
				if enabled, err := db.DailyReportEnabled(ctx, user); err != nil || enabled || len(edits) != 1 {
					t.Fatalf("blocked recipient must be unsubscribed: %t / %v / %v", enabled, err, edits)
				}
			} else if len(edits) != 2 || !strings.Contains(edits[1], `"message_id":551`) || fc.callCount("sendMessage") != 1 {
				t.Fatalf("retry failed to edit original message: %v / %v", edits, fc.callList())
			}
		})
	}
}

func TestReportLine(t *testing.T) {
	tests := []struct {
		name string
		c    storage.ChatInfo
		s    storage.Stats
		want string
	}{
		{"empty day", storage.ChatInfo{ChatID: -5, Title: "Бег"}, storage.Stats{},
			"«Бег» — без событий"},
		{"normal day", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{Joined: 5, Passed: 4, Kicked: 1},
			"«Бег» — вступило 5, прошло 4, вышли сами 0, кик 1, бан 0, сообщений: 0"},
		{"bans merge spam", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{Banned: 1, SpamBanned: 2},
			"«Бег» — вступило 0, прошло 0, вышли сами 0, кик 0, бан 3 (из них спам 2), сообщений: 0"},
		{"title escaped", storage.ChatInfo{ChatID: -5, Title: "A<b>&"}, storage.Stats{Passed: 1},
			"«A&lt;b&gt;&amp;» — вступило 0, прошло 1, вышли сами 0, кик 0, бан 0, сообщений: 0"},
		{"public chat linked", storage.ChatInfo{ChatID: -5, Title: "Бег", Username: "run_chat"},
			storage.Stats{Passed: 1},
			`<a href="https://t.me/run_chat">«Бег»</a> — вступило 0, прошло 1, вышли сами 0, кик 0, бан 0, сообщений: 0`},
		{"left counted", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{Joined: 5, Passed: 3, Kicked: 1, Left: 1},
			"«Бег» — вступило 5, прошло 3, вышли сами 1, кик 1, бан 0, сообщений: 0"},
		{"messages only is not empty", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{MsgOldtimer: 12},
			"«Бег» — сообщений: 12, событий воронки нет"},

		{"pending shown", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{Joined: 7, Kicked: 5, Left: 1, Pending: 1},
			"«Бег» — вступило 7, прошло 0, вышли сами 1, кик 5, бан 0, в процессе 1, сообщений: 0"},
		// Сообщения отчёта идут всегда, даже когда событий воронки нет числами.
		{"messages always appended", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{Joined: 2, Passed: 1, Pending: 1, MsgNewcomer: 3, MsgOldtimer: 100},
			"«Бег» — вступило 2, прошло 1, вышли сами 0, кик 0, бан 0, в процессе 1, сообщений: 103"},
		// Модерация старожилов не затрагивает ожидания новичков.
		{"moderation outside the funnel", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{Passed: 1, ModKicked: 3},
			"«Бег» — вступило 0, прошло 1, вышли сами 0, кик 0, бан 0, админ-команды 3, сообщений: 0"},
		{"aborted closes funnel", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{Joined: 2, Aborted: 1, Pending: 1, MsgOldtimer: 5},
			"«Бег» — вступило 2, прошло 0, вышли сами 0, кик 0, бан 0, не проверено 1, в процессе 1, сообщений: 5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reportLine(tc.c, tc.s); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestDigestHasContent(t *testing.T) {
	if digestHasContent(storage.Stats{}, nil, nil, nil, nil) {
		t.Fatal("fully empty day must skip the digest")
	}
	// День, где единственная активность — прохождение капчи (join остался за
	// границей окна: вошёл 23:59, прошёл 00:01).
	if !digestHasContent(storage.Stats{Passed: 1}, nil, nil,
		[]storage.UserCount{{UserID: 1}}, nil) {
		t.Fatal("pass-only day must produce a digest")
	}
	// День, где был только бан ИИ-антиспамом.
	if !digestHasContent(storage.Stats{SpamBanned: 1}, nil, nil, nil,
		[]storage.UserCount{{UserID: 2}}) {
		t.Fatal("spamban-only day must produce a digest")
	}
	if !digestHasContent(storage.Stats{MsgOldtimer: 5}, nil, nil, nil, nil) {
		t.Fatal("messages-only day must produce a digest")
	}
	// День, где единственное событие — юзер вышел посреди капчи.
	if !digestHasContent(storage.Stats{Left: 1}, nil, nil, nil, nil) {
		t.Fatal("left-only day must produce a digest")
	}
	if !digestHasContent(storage.Stats{}, nil, []storage.UserCount{{UserID: 3}}, nil, nil) {
		t.Fatal("failers-only day must produce a digest")
	}
}

// TestDigestHasContentCoversAllStatsCounters — страж от дрейфа: новый
// счётчик в storage.Stats, забытый в digestHasContent, приводил бы к тихо
// пропущенным дайджестам (SpamBanned уже однажды так потерялся).
func TestDigestHasContentCoversAllStatsCounters(t *testing.T) {
	typ := reflect.TypeOf(storage.Stats{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Int {
			continue // PeriodFrom/PeriodUntil — не счётчики
		}
		var s storage.Stats
		reflect.ValueOf(&s).Elem().Field(i).SetInt(1)
		if !digestHasContent(s, nil, nil, nil, nil) {
			t.Errorf("Stats.%s=1 must make the digest non-empty — add it to digestHasContent", f.Name)
		}
	}
}

func TestDMReportChecksFreshRolesAndRetriesOneMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.db")
	b, fc := replyAnchorBot(t, path)
	ctx := b.runCtx
	const user = int64(701)
	const former, current, delayed int64 = -10001, -10002, -10003
	for i, id := range []int64{former, current, delayed} {
		serviceableChat(t, b, b.db, id)
		titles := []string{"A Former", "B Current", "C Delayed"}
		if err := b.db.RememberChat(ctx, storage.ChatInfo{ChatID: id, Title: titles[i], Type: "supergroup"}); err != nil {
			t.Fatal(err)
		}
		b.cacheAdmin(id, user, true)
	}
	if err := b.db.SetDailyReport(ctx, user, true); err != nil {
		t.Fatal(err)
	}
	const member = `{"status":"member","user":{"id":701,"is_bot":false,"first_name":"Test"}}`
	const admin = `{"status":"administrator","user":{"id":701,"is_bot":false,"first_name":"Test"}}`
	fc.respSeq["getChatMember"] = []string{member, admin, admin, member, admin, admin}
	unavailable := &telegoapi.Error{ErrorCode: 500, Description: "temporary"}
	fc.errSeq["getChatMember"] = []*telegoapi.Error{nil, nil, unavailable, nil, nil, unavailable}
	fc.resp["sendMessage"] = privateSentJSON(user, 551)

	now := time.Date(2030, 6, 2, 12, 0, 0, 0, storage.StatsLocation)
	from, until := statsRange(periodYesterday, now)
	day := storage.DayOf(now)
	b.maybeSendDMReports(ctx, now, from, until, day)
	b.maybeSendDMReports(ctx, now, from, until, day)
	sends := fc.callBodies("sendMessage")
	if len(sends) != 1 || !strings.Contains(sends[0], `"chat_id":701`) ||
		!strings.Contains(sends[0], "B Current") ||
		strings.Contains(sends[0], "A Former") || strings.Contains(sends[0], "C Delayed") {
		t.Fatalf("incorrect partial report: %v", sends)
	}
	if fc.callCount("getChatMember") != 6 || fc.callCount("editMessageText") != 0 {
		t.Fatalf("repeat did not recheck roles or edited an identical report: %v", fc.callList())
	}

	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, caller := replyAnchorBot(t, path)
	ctx = restarted.runCtx
	caller.respSeq["getChatMember"] = []string{member, admin, admin, member, admin, admin}
	caller.errSeq["editMessageText"] = []*telegoapi.Error{unavailable, nil}
	caller.resp["editMessageText"] = privateSentJSON(user, 551)
	for range 3 {
		restarted.maybeSendDMReports(ctx, now, from, until, day)
	}
	edits := caller.callBodies("editMessageText")
	if caller.callCount("sendMessage") != 0 || len(edits) != 2 || edits[0] != edits[1] ||
		!strings.Contains(edits[1], `"message_id":551`) || strings.Contains(edits[1], "A Former") ||
		!strings.Contains(edits[1], "B Current") || !strings.Contains(edits[1], "C Delayed") {
		t.Fatalf("retry created a copy or edited the wrong report: %v / %v", caller.callList(), edits)
	}
	subs, err := restarted.db.DailyReportSubscribers(ctx)
	if err != nil || len(subs) != 1 || subs[0].LastDay != day {
		t.Fatalf("report was not recorded as complete: %v / %v", subs, err)
	}
}

func TestOwnerDMReportDoesNotDependOnChatRole(t *testing.T) {
	b, db, fc := newFlowBot(t)
	ctx := b.runCtx
	const owner = int64(702)
	b.cfg.OwnerIDs = map[int64]struct{}{owner: {}}
	serviceableChat(t, b, db, testChatID)
	fc.err["getChatMember"] = &telegoapi.Error{ErrorCode: 500, Description: "temporary"}
	now := time.Date(2030, 6, 2, 12, 0, 0, 0, storage.StatsLocation)
	from, until := statsRange(periodYesterday, now)
	b.sendDMReport(ctx, owner, from, until, storage.DayOf(now))
	if fc.callCount("getChatMember") != 0 || fc.callCount("sendMessage") != 1 {
		t.Fatalf("owner report incorrectly depended on chat roles: %v", fc.callList())
	}
}
