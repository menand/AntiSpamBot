package bot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

const (
	testOwnerID     = 900
	testProbeChatID = -100200
)

func newReplyWatchBot(t *testing.T) (*Bot, *storage.DB, *fakeCaller) {
	t.Helper()
	b, db, fc := newFlowBot(t)
	b.cfg.ReplyWatchEnabled = true
	b.cfg.ReplyWatchChatID = testProbeChatID
	b.cfg.OwnerIDs = map[int64]struct{}{testOwnerID: {}}
	serviceableChat(t, b, db, testChatID)
	fc.resp["copyMessage"] = `{"message_id":9001}`
	return b, db, fc
}

func putReplyWatch(t *testing.T, db *storage.DB, w storage.ReplyWatch) {
	t.Helper()
	if err := db.PutReplyWatch(context.Background(), w); err != nil {
		t.Fatalf("put reply watch: %v", err)
	}
}

func TestNextReplyWatchProbe(t *testing.T) {
	answered := time.Unix(1_700_000_000, 0)
	mustTime := func(d time.Duration) time.Time { return answered.Add(d) }

	cases := []struct {
		name   string
		stage  int
		now    time.Time
		wantOK bool
		wantSt int
		wantAt time.Time
	}{
		{"early re-arms same stage", 0, mustTime(30 * time.Second), true, 0, mustTime(time.Minute)},
		{"exact first", 0, mustTime(time.Minute), true, 1, mustTime(5 * time.Minute)},
		{"before five", 0, mustTime(4 * time.Minute), true, 1, mustTime(5 * time.Minute)},
		{"between five and fifteen", 0, mustTime(6 * time.Minute), true, 2, mustTime(15 * time.Minute)},
		{"between fifteen and sixty", 0, mustTime(20 * time.Minute), true, 3, mustTime(60 * time.Minute)},
		{"after sixty", 0, mustTime(61 * time.Minute), false, 0, time.Time{}},
		{"final stage at sixty", 3, mustTime(60 * time.Minute), false, 0, time.Time{}},
		{"stage two skips to sixty", 2, mustTime(16 * time.Minute), true, 3, mustTime(60 * time.Minute)},
		{"stale stage", 8, mustTime(time.Minute), false, 0, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, at, ok := nextReplyWatchProbe(answered, tc.stage, tc.now)
			if ok != tc.wantOK || st != tc.wantSt || (ok && !at.Equal(tc.wantAt)) {
				t.Fatalf("nextReplyWatchProbe = (%d, %s, %v), want (%d, %s, %v)",
					st, at, ok, tc.wantSt, tc.wantAt, tc.wantOK)
			}
		})
	}
}

func TestClassifyReplyProbeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want replyProbeResult
	}{
		{"nil is alive", nil, replyProbeAlive},
		{"copy not found is deleted", &telegoapi.Error{
			ErrorCode: 400, Description: "Bad Request: message to copy not found",
		}, replyProbeDeleted},
		{"protected content is unknown", &telegoapi.Error{
			ErrorCode: 400, Description: "Bad Request: message can't be copied",
		}, replyProbeUnknown},
		{"not member is target unavailable", &telegoapi.Error{
			ErrorCode: 403, Description: "Forbidden: bot is not a member of the chat",
		}, replyProbeTargetUnavailable},
		{"network is unknown", errors.New("i/o timeout"), replyProbeRetry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyReplyProbeError(tc.err); got != tc.want {
				t.Fatalf("classifyReplyProbeError = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStartReplyWatchAndDisabled(t *testing.T) {
	b, db, _ := newReplyWatchBot(t)
	msg := &telego.Message{
		MessageID: 100,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup", Title: "Тест"},
		From:      &telego.User{ID: testUserID, FirstName: "Иван"},
		Text:      "планирую бегать",
	}
	b.startReplyWatch(msg)

	w, ok, err := db.GetReplyWatch(context.Background(), testChatID, 100)
	if err != nil || !ok {
		t.Fatalf("watch must exist: ok=%v err=%v", ok, err)
	}
	if w.Text != "планирую бегать" || w.UserID != testUserID || w.Stage != 0 {
		t.Fatalf("unexpected watch: %+v", w)
	}
	if d := time.Until(w.NextAt); d < 55*time.Second || d > 75*time.Second {
		t.Fatalf("first probe in %s, want ~1m (60s + jitter 0..10s)", d)
	}

	b.cfg.ReplyWatchEnabled = false
	b.startReplyWatch(&telego.Message{
		MessageID: 101,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		From:      &telego.User{ID: testUserID},
		Text:      "ещё ответ",
	})
	if _, ok, _ := db.GetReplyWatch(context.Background(), testChatID, 101); ok {
		t.Fatal("disabled feature must not create a watch")
	}
}

func TestReplyWatchProbeAliveAdvancesAndDeletesCopy(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	answered := time.Now().Add(-2 * time.Minute)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "живой ответ", AnsweredAt: answered,
		Stage: 0, NextAt: time.Now().Add(-time.Second),
	})

	b.probeDueReplyWatches(context.Background())

	w, ok, err := db.GetReplyWatch(context.Background(), testChatID, 100)
	if err != nil || !ok {
		t.Fatalf("watch must survive an alive probe: ok=%v err=%v", ok, err)
	}
	if w.Stage != 1 {
		t.Fatalf("stage = %d, want 1", w.Stage)
	}
	// SQLite хранит Unix-секунды — сравниваем с точностью до секунды.
	if w.NextAt.Unix() != answered.Add(5*time.Minute).Unix() {
		t.Fatalf("next_at = %s, want %s", w.NextAt, answered.Add(5*time.Minute))
	}
	if n := fc.callCount("copyMessage"); n != 1 {
		t.Fatalf("copyMessage calls = %d, want 1", n)
	}
	if n := fc.callCount("deleteMessage"); n != 1 {
		t.Fatalf("deleteMessage calls = %d, want 1 (probe copy)", n)
	}

	var copyBody struct {
		ChatID              int64 `json:"chat_id"`
		FromChatID          int64 `json:"from_chat_id"`
		MessageID           int   `json:"message_id"`
		DisableNotification bool  `json:"disable_notification"`
	}
	if err := json.Unmarshal([]byte(fc.callBodies("copyMessage")[0]), &copyBody); err != nil {
		t.Fatalf("decode copy body: %v", err)
	}
	if copyBody.ChatID != testProbeChatID || copyBody.FromChatID != testChatID ||
		copyBody.MessageID != 100 || !copyBody.DisableNotification {
		t.Fatalf("unexpected copy body: %+v", copyBody)
	}
	var delBody struct {
		ChatID    int64 `json:"chat_id"`
		MessageID int   `json:"message_id"`
	}
	if err := json.Unmarshal([]byte(fc.callBodies("deleteMessage")[0]), &delBody); err != nil {
		t.Fatalf("decode delete body: %v", err)
	}
	if delBody.ChatID != testProbeChatID || delBody.MessageID != 9001 {
		t.Fatalf("unexpected delete body: %+v", delBody)
	}
}

func TestReplyWatchProbeDeletedNotifiesAndTakesRow(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "удалённый ответ", AnsweredAt: time.Now().Add(-5 * time.Minute),
		Stage: 0, NextAt: time.Now().Add(-time.Second),
	})
	fc.err["copyMessage"] = &telegoapi.Error{
		ErrorCode: 400, Description: "Bad Request: message to copy not found",
	}

	b.probeDueReplyWatches(context.Background())

	if _, ok, _ := db.GetReplyWatch(context.Background(), testChatID, 100); ok {
		t.Fatal("deleted watch must be taken")
	}
	if n := fc.callCount("sendMessage"); n != 1 {
		t.Fatalf("owner notifications = %d, want 1", n)
	}
	body := fc.callBodies("sendMessage")[0]
	for _, want := range []string{"Сообщение удалено", "не наш бот", "через 5 минут", "удалённый ответ"} {
		if !strings.Contains(body, want) {
			t.Fatalf("notification body missing %q: %s", want, body)
		}
	}
}

func TestReplyWatchEditCardsThrottledAndCapped(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "версия 0", AnsweredAt: time.Now().Add(-10 * time.Minute),
		Stage: 1, NextAt: time.Now().Add(3 * time.Minute),
	})
	b.replyWatchChats.Store(int64(testChatID), struct{}{})
	edit := func(text string) {
		b.maybeHandleReplyWatchEdit(&telego.Message{
			MessageID: 100,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			From:      &telego.User{ID: testUserID, FirstName: "Иван"},
			Text:      text,
		})
	}

	edit("версия 1")
	waitForWithin(t, 3*time.Second, func() bool { return fc.callCount("sendMessage") >= 1 })
	w, _, _ := db.GetReplyWatch(context.Background(), testChatID, 100)
	if w.Text != "версия 1" || w.EditReports != 1 {
		t.Fatalf("first edit: %+v", w)
	}

	// Вторая правка в кулдауне: снимок обновляется, карточки нет.
	edit("версия 2")
	time.Sleep(100 * time.Millisecond)
	if n := fc.callCount("sendMessage"); n != 1 {
		t.Fatalf("cooldown edit sent %d cards, want 1 total", n)
	}
	w, _, _ = db.GetReplyWatch(context.Background(), testChatID, 100)
	if w.Text != "версия 2" || w.EditReports != 1 {
		t.Fatalf("cooldown edit must update snapshot only: %+v", w)
	}

	// Лимит исчерпан: даже спустя кулдаун карточки нет, снимок — свежий.
	w.EditReports = replyWatchEditMaxReports
	w.EditNotifiedAt = time.Now().Add(-time.Hour)
	putReplyWatch(t, db, w)
	edit("версия 3")
	time.Sleep(100 * time.Millisecond)
	if n := fc.callCount("sendMessage"); n != 1 {
		t.Fatalf("capped edit sent %d cards, want 1 total", n)
	}
	w, _, _ = db.GetReplyWatch(context.Background(), testChatID, 100)
	if w.Text != "версия 3" || w.EditReports != replyWatchEditMaxReports {
		t.Fatalf("capped edit must update snapshot only: %+v", w)
	}
}

func TestReplyWatchDeletionReportRetriesAfterSendFailure(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "удалённый ответ", AnsweredAt: time.Now().Add(-5 * time.Minute),
		Stage: 0, NextAt: time.Now().Add(-time.Second),
	})
	fc.err["copyMessage"] = &telegoapi.Error{
		ErrorCode: 400, Description: "Bad Request: message to copy not found",
	}
	fc.err["sendMessage"] = &telegoapi.Error{
		ErrorCode: 500, Description: "Internal Server Error",
	}

	b.probeDueReplyWatches(context.Background())

	w, ok, err := db.GetReplyWatch(context.Background(), testChatID, 100)
	if err != nil || !ok {
		t.Fatalf("undelivered report must requeue the watch: ok=%v err=%v", ok, err)
	}
	if w.ReportAttempts != 1 {
		t.Fatalf("report_attempts = %d, want 1", w.ReportAttempts)
	}
	if d := time.Until(w.NextAt); d < 4*time.Minute || d > 6*time.Minute {
		t.Fatalf("retry scheduled in %s, want ~5m", d)
	}

	// Вторая попытка: отправка починилась, строка снимается.
	fc.err["sendMessage"] = nil
	if err := db.DeferReplyWatch(context.Background(), testChatID, 100,
		time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	b.probeDueReplyWatches(context.Background())
	if _, ok, _ := db.GetReplyWatch(context.Background(), testChatID, 100); ok {
		t.Fatal("delivered report must take the watch")
	}
	if n := fc.callCount("sendMessage"); n != 2 {
		t.Fatalf("sendMessage calls = %d, want 2 (failed + successful retry)", n)
	}
}

func TestReplyWatchFloodControlDefersWithoutAdvancing(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "ответ", AnsweredAt: time.Now().Add(-2 * time.Minute),
		Stage: 0, NextAt: time.Now().Add(-time.Second),
	})
	fc.err["copyMessage"] = &telegoapi.Error{
		ErrorCode: 429, Description: "Too Many Requests: retry after 10",
		Parameters: &telegoapi.ResponseParameters{RetryAfter: 10},
	}

	b.probeDueReplyWatches(context.Background())

	w, ok, err := db.GetReplyWatch(context.Background(), testChatID, 100)
	if err != nil || !ok {
		t.Fatalf("429 must keep the watch: ok=%v err=%v", ok, err)
	}
	if w.Stage != 0 {
		t.Fatalf("429 must not advance the stage: %d", w.Stage)
	}
	if d := time.Until(w.NextAt); d < 30*time.Second || d > 90*time.Second {
		t.Fatalf("429 retry scheduled in %s, want ~1m", d)
	}
	if n := fc.callCount("sendMessage"); n != 0 {
		t.Fatalf("429 sent %d reports, want 0", n)
	}
}

func TestReplyWatchUnknownDoesNotReportDeletion(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "защищённый ответ", AnsweredAt: time.Now().Add(-2 * time.Minute),
		Stage: 0, NextAt: time.Now().Add(-time.Second),
	})
	fc.err["copyMessage"] = &telegoapi.Error{
		ErrorCode: 400, Description: "Bad Request: message can't be copied",
	}

	b.probeDueReplyWatches(context.Background())

	w, ok, err := db.GetReplyWatch(context.Background(), testChatID, 100)
	if err != nil || !ok {
		t.Fatalf("unknown probe must not take the watch: ok=%v err=%v", ok, err)
	}
	if w.Stage != 1 {
		t.Fatalf("stage = %d, want 1", w.Stage)
	}
	if n := fc.callCount("sendMessage"); n != 0 {
		t.Fatalf("unknown probe sent %d notifications, want 0", n)
	}
}

func TestReplyWatchBrokenServiceChatFallsBackToOwnerDM(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "ответ", AnsweredAt: time.Now().Add(-2 * time.Minute),
		Stage: 0, NextAt: time.Now().Add(-time.Second),
	})
	fc.errSeq["copyMessage"] = []*telegoapi.Error{
		{ErrorCode: 400, Description: "Bad Request: chat not found"},
		nil,
	}
	fc.err["getChat"] = &telegoapi.Error{
		ErrorCode: 400, Description: "Bad Request: chat not found",
	}

	b.probeDueReplyWatches(context.Background())

	w, ok, err := db.GetReplyWatch(context.Background(), testChatID, 100)
	if err != nil || !ok {
		t.Fatalf("fallback probe must keep the watch: ok=%v err=%v", ok, err)
	}
	if w.Stage != 1 {
		t.Fatalf("stage = %d, want 1", w.Stage)
	}
	if n := fc.callCount("copyMessage"); n != 2 {
		t.Fatalf("copyMessage calls = %d, want 2 (service + owner fallback)", n)
	}
	bodies := fc.callBodies("copyMessage")
	var fallback struct {
		ChatID int64 `json:"chat_id"`
	}
	if err := json.Unmarshal([]byte(bodies[1]), &fallback); err != nil {
		t.Fatalf("decode fallback body: %v", err)
	}
	if fallback.ChatID != testOwnerID {
		t.Fatalf("fallback chat_id = %d, want owner %d", fallback.ChatID, testOwnerID)
	}
	if n := fc.callCount("sendMessage"); n != 1 {
		t.Fatalf("outage notifications = %d, want 1", n)
	}
	if body := fc.callBodies("sendMessage")[0]; !strings.Contains(body, "недоступен") {
		t.Fatalf("outage notification body = %s", body)
	}
}

func TestReplyWatchEditUpdatesSnapshotAndNotifies(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "старое", AnsweredAt: time.Now().Add(-2 * time.Minute),
		Stage: 1, NextAt: time.Now().Add(3 * time.Minute),
	})
	b.replyWatchChats.Store(int64(testChatID), struct{}{})

	b.maybeHandleReplyWatchEdit(&telego.Message{
		MessageID: 100,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		From:      &telego.User{ID: testUserID, FirstName: "Иван"},
		Text:      "новое",
	})

	w, ok, err := db.GetReplyWatch(context.Background(), testChatID, 100)
	t.Logf("DEBUG after edit: ok=%v err=%v text=%q stage=%d editReports=%d", ok, err, w.Text, w.Stage, w.EditReports)
	if err != nil || !ok {
		t.Fatalf("watch must survive edit: ok=%v err=%v", ok, err)
	}
	if w.Text != "новое" {
		t.Fatalf("snapshot = %q, want %q (editReports=%d stage=%d)", w.Text, "новое", w.EditReports, w.Stage)
	}
	waitForWithin(t, 3*time.Second, func() bool { return fc.callCount("sendMessage") >= 1 })
	body := fc.callBodies("sendMessage")[0]
	for _, want := range []string{"Сообщение отредактировано", "старое", "новое"} {
		if !strings.Contains(body, want) {
			t.Fatalf("edit notification body missing %q: %s", want, body)
		}
	}
}

func TestReplyWatchBotDeletionMarkIsReported(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "ответ", AnsweredAt: time.Now().Add(-5 * time.Minute),
		Stage: 0, NextAt: time.Now().Add(-time.Second),
	})
	b.markReplyWatchDeletedByMessage(testChatID, 100, replyWatchDeletedByDel)
	fc.err["copyMessage"] = &telegoapi.Error{
		ErrorCode: 400, Description: "Bad Request: message to copy not found",
	}

	b.probeDueReplyWatches(context.Background())

	if n := fc.callCount("sendMessage"); n != 1 {
		t.Fatalf("owner notifications = %d, want 1", n)
	}
	if body := fc.callBodies("sendMessage")[0]; !strings.Contains(body, "бот (команда /del)") {
		t.Fatalf("deletion notification missing bot reason: %s", body)
	}
}

func TestReplyWatchSurvivesSixtyMinutesSilently(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "долгожитель", AnsweredAt: time.Now().Add(-61 * time.Minute),
		Stage: 3, NextAt: time.Now().Add(-time.Second),
	})

	b.probeDueReplyWatches(context.Background())

	if _, ok, _ := db.GetReplyWatch(context.Background(), testChatID, 100); ok {
		t.Fatal("watch must be removed after the final probe")
	}
	if n := fc.callCount("sendMessage"); n != 0 {
		t.Fatalf("survived message sent %d notifications, want 0", n)
	}
}

func TestReplyWatchRestartCatchUpSkipsMissedProbes(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	answered := time.Now().Add(-30 * time.Minute)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "ответ", AnsweredAt: answered,
		Stage: 0, NextAt: time.Now().Add(-29 * time.Minute),
	})

	b.probeDueReplyWatches(context.Background())

	w, ok, err := db.GetReplyWatch(context.Background(), testChatID, 100)
	if err != nil || !ok {
		t.Fatalf("watch must survive catch-up: ok=%v err=%v", ok, err)
	}
	if w.Stage != 3 {
		t.Fatalf("stage = %d, want 3 (next future probe is 60m)", w.Stage)
	}
	if w.NextAt.Unix() != answered.Add(60*time.Minute).Unix() {
		t.Fatalf("next_at = %s, want %s", w.NextAt, answered.Add(60*time.Minute))
	}
	_ = fc
}

func TestReplyWaitMiddlewareStartsWatch(t *testing.T) {
	b, db, _ := newReplyWatchBot(t)
	b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
	m := &telego.Message{
		MessageID: 100,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup", Title: "Тест"},
		From:      &telego.User{ID: testUserID, FirstName: "Иван"},
		Text:      "планирую бегать",
	}
	if b.replyWaitMiddleware(m) {
		t.Fatal("meaningful answer must not be swallowed")
	}
	w, ok, err := db.GetReplyWatch(context.Background(), testChatID, 100)
	if err != nil || !ok {
		t.Fatalf("middleware must create a watch: ok=%v err=%v", ok, err)
	}
	if w.Text != "планирую бегать" {
		t.Fatalf("watch text = %q", w.Text)
	}
}

func TestReplyWatchSourceUnavailableIsUnknown(t *testing.T) {
	b, db, fc := newReplyWatchBot(t)
	putReplyWatch(t, db, storage.ReplyWatch{
		ChatID: testChatID, UserID: testUserID, MessageID: 100,
		Text: "ответ", AnsweredAt: time.Now().Add(-2 * time.Minute),
		Stage: 0, NextAt: time.Now().Add(-time.Second),
	})
	fc.err["copyMessage"] = &telegoapi.Error{
		ErrorCode: 400, Description: "Bad Request: message to copy not found",
	}
	// Бот выгнан из чата-источника: тот же «not found», но это не удаление.
	fc.resp["getChatMember"] = `{"status":"left","user":{"id":42,"is_bot":true,"first_name":"Test"}}`

	b.probeDueReplyWatches(context.Background())

	w, ok, err := db.GetReplyWatch(context.Background(), testChatID, 100)
	if err != nil || !ok {
		t.Fatalf("source outage must not take the watch: ok=%v err=%v", ok, err)
	}
	if w.Stage != 1 {
		t.Fatalf("stage = %d, want 1", w.Stage)
	}
	if n := fc.callCount("sendMessage"); n != 0 {
		t.Fatalf("source outage sent %d notifications, want 0", n)
	}
}

func TestStartReplyWatchRequiresOwner(t *testing.T) {
	b, db, _ := newReplyWatchBot(t)
	b.cfg.OwnerIDs = nil
	b.startReplyWatch(&telego.Message{
		MessageID: 100,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		From:      &telego.User{ID: testUserID},
		Text:      "ответ",
	})
	if _, ok, _ := db.GetReplyWatch(context.Background(), testChatID, 100); ok {
		t.Fatal("without owners there is nowhere to report — no watch expected")
	}
}

func TestTruncateUTF16(t *testing.T) {
	got := truncateUTF16(strings.Repeat("😀", 10), 5)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated string = %q, want ellipsis", got)
	}
	if units := len(utf16.Encode([]rune(got))); units > 5 {
		t.Fatalf("truncated string has %d UTF-16 units, want <= 5", units)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncated string contains replacement rune: %q", got)
	}
}

func TestReplyWatchQuoteEscapesAndCutsEntities(t *testing.T) {
	// 2000 амперсандов после экранирования дают ~10000 UTF-16 единиц;
	// сырой лимит тут не спасает — резать надо готовый HTML, не разрывая
	// entity (иначе Telegram отвергнет «&am…»).
	got := replyWatchQuote(strings.Repeat("&", 2000))
	inner := strings.TrimSuffix(strings.TrimPrefix(got, "<blockquote>"), "</blockquote>")
	if units := len(utf16.Encode([]rune(inner))); units > replyWatchQuoteLimit {
		t.Fatalf("quote has %d UTF-16 units, want <= %d", units, replyWatchQuoteLimit)
	}
	for i := 0; i < len(inner); i++ {
		if inner[i] != '&' {
			continue
		}
		if !strings.ContainsRune(inner[i:], ';') {
			t.Fatalf("quote contains a broken HTML entity: %q", inner)
		}
	}
	if strings.ContainsAny(inner, "<>") {
		t.Fatalf("quote leaked unescaped markup: %q", inner)
	}
	if got := replyWatchQuote("  "); got != "<blockquote><i>— (текст убран)</i></blockquote>" {
		t.Fatalf("empty quote = %q", got)
	}
}

func TestReplyWatchDeletedByLabels(t *testing.T) {
	cases := map[string]string{
		replyWatchDeletedByDel:        "бот (команда /del)",
		replyWatchDeletedByKick:       "бот (команда /kick)",
		replyWatchDeletedByBan:        "бот (команда /ban)",
		replyWatchDeletedBySpam:       "бот (спам-бан)",
		replyWatchDeletedByQuarantine: "бот (карантин)",
		replyWatchDeletedByCaptcha:    "бот (капча)",
		"":                            "не наш бот (юзер, админ или другой бот — неразличимо)",
		"bot:future":                  "бот (неизвестное действие)",
	}
	for reason, want := range cases {
		if got := replyWatchDeletedBy(reason); got != want {
			t.Fatalf("replyWatchDeletedBy(%q) = %q, want %q", reason, got, want)
		}
	}
}
