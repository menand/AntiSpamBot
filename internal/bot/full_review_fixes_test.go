package bot

// Тесты раунда полного ревью: пробелы покрытия, помеченные тестером
// (ветвление deleteBotMessage, границ truncateLabel, astral-entities,
// trust-гейт бюллетеней на sv:-пути), плюс регресс дедупа поздней
// дубль-доставки join после уже решённой капчи.

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// TestDeleteBotMessageBranching — пин диспатча deleteBotMessage: ephID == 0
// обязан идти обычным deleteMessage по msgID, ephID != 0 — через
// DeleteEphemeralMessage с receiver'ом (CLAUDE.md: «branches on ephID != 0»).
func TestDeleteBotMessageBranching(t *testing.T) {
	t.Run("обычное сообщение", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		if err := b.deleteBotMessage(context.Background(), testChatID, 555, 0, 0); err != nil {
			t.Fatal(err)
		}
		if n := fc.callCount("deleteMessage"); n != 1 {
			t.Fatalf("deleteMessage calls = %d, want 1", n)
		}
		if n := fc.callCount("deleteEphemeralMessage"); n != 0 {
			t.Fatalf("deleteEphemeralMessage calls = %d, want 0", n)
		}
		bodies := fc.callBodies("deleteMessage")
		var body map[string]any
		if err := json.Unmarshal([]byte(bodies[0]), &body); err != nil {
			t.Fatal(err)
		}
		if body["message_id"] != float64(555) {
			t.Fatalf("message_id = %v, want 555", body["message_id"])
		}
	})

	t.Run("эфемерное сообщение", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		if err := b.deleteBotMessage(context.Background(), testChatID, 0, 42, 7); err != nil {
			t.Fatal(err)
		}
		if n := fc.callCount("deleteEphemeralMessage"); n != 1 {
			t.Fatalf("deleteEphemeralMessage calls = %d, want 1", n)
		}
		if n := fc.callCount("deleteMessage"); n != 0 {
			t.Fatalf("deleteMessage calls = %d, want 0", n)
		}
		bodies := fc.callBodies("deleteEphemeralMessage")
		var body map[string]any
		if err := json.Unmarshal([]byte(bodies[0]), &body); err != nil {
			t.Fatal(err)
		}
		if body["ephemeral_message_id"] != float64(42) || body["receiver_user_id"] != float64(7) {
			t.Fatalf("ephemeral body = %v, want ephemeral_message_id=42 receiver_user_id=7", body)
		}
	})

	t.Run("обёртки ошибок", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		fc.err["deleteMessage"] = &telegoapi.Error{ErrorCode: 400, Description: "nope"}
		err := b.deleteBotMessage(context.Background(), testChatID, 555, 0, 0)
		if err == nil || !strings.Contains(err.Error(), "delete message") {
			t.Fatalf("regular error must wrap «delete message», got %v", err)
		}

		fc.err["deleteEphemeralMessage"] = &telegoapi.Error{ErrorCode: 400, Description: "nope"}
		err = b.deleteBotMessage(context.Background(), testChatID, 0, 42, 7)
		if err == nil || !strings.Contains(err.Error(), "delete ephemeral message") {
			t.Fatalf("ephemeral error must wrap «delete ephemeral message», got %v", err)
		}
	})
}

// TestTruncateLabelTable — границы rune-safe обрезки: точная длина не должна
// получать многоточие, срез не должен ломать astral-руны (эмодзи из 4 байт),
// пустая строка проходит насквозь.
func TestTruncateLabelTable(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		max     int
		wantOut string // "" = проверить свойства (длина/многоточие/префикс)
	}{
		{name: "точная длина без изменений и без многоточия", in: "Название чата", max: 13,
			wantOut: "Название чата"},
		{name: "на единицу длиннее — max рун с многоточием", in: "Очень длинное название чата", max: 10},
		{name: "эмодзи режется по рунам, не по байтам", in: "🤖💥Чат с очень длинным названием канала", max: 8},
		{name: "короткая строка насквозь", in: "Чат", max: 40, wantOut: "Чат"},
		{name: "пустая строка насквозь", in: "", max: 5, wantOut: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateLabel(tc.in, tc.max)
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8: %q", got)
			}
			if tc.wantOut != "" || tc.in == "" {
				if got != tc.wantOut {
					t.Fatalf("got %q, want %q", got, tc.wantOut)
				}
				return
			}
			runes := []rune(got)
			if len(runes) != tc.max {
				t.Fatalf("len(runes) = %d, want %d (%q)", len(runes), tc.max, got)
			}
			if !strings.HasSuffix(got, "…") {
				t.Fatalf("expected ellipsis suffix, got %q", got)
			}
			if prefix := string(runes[:len(runes)-1]); prefix != string([]rune(tc.in)[:tc.max-1]) {
				t.Fatalf("prefix mismatch: %q vs input %q", prefix, tc.in)
			}
		})
	}
}

// TestEntitiesToHTMLAstral — utf16-математика вокруг surrogate pair: entity,
// накрывающая эмодзи целиком (2 юнита = 1 руна), переполнение Length
// (регресс overflow-фикса клампа) и пересечение границ текста.
func TestEntitiesToHTMLAstral(t *testing.T) {
	tests := []struct {
		name string
		text string
		ents []telego.MessageEntity
		want string
	}{
		{
			// 😀 = 2 UTF-16 юнита, но 1 руна: bold длиной 2 юнита покрывает
			// её ровно, «ок» остаётся снаружи.
			name: "bold поверх эмодзи",
			text: "😀ок",
			ents: []telego.MessageEntity{ent(telego.EntityTypeBold, 0, 2)},
			want: "<b>😀</b>ок",
		},
		{
			// Регресс переполнения: Offset+Length у MaxInt64 уходил в минус,
			// кламп пропускал, дальше падал utf16.Decode slice-bounds panic.
			name: "гигантская Length клампится без переполнения",
			text: "аб",
			ents: []telego.MessageEntity{ent(telego.EntityTypeBold, 0, math.MaxInt64)},
			want: "<b>аб</b>",
		},
		{
			name: "entity полностью за концом текста отбрасывается",
			text: "аб",
			ents: []telego.MessageEntity{ent(telego.EntityTypeBold, 5, 3)},
			want: "аб",
		},
		{
			// Telegram такие не шлёт («вложены либо не пересекаются»), но
			// crafted JSON не должен ронять рендер: пересекающаяся entity
			// отбрасывается фильтром — вывод остаётся сбалансированным.
			name: "пересекающиеся same-type: лишняя отброшена",
			text: "абвгде",
			ents: []telego.MessageEntity{
				ent(telego.EntityTypeBold, 0, 4),
				ent(telego.EntityTypeBold, 2, 4),
			},
			want: "<b>абвг</b>де",
		},
		{
			// Регресс «тройки»: underline пересекает bold, но против
			// вложенного italic парный фильтр пересечения бы не увидел —
			// фильтр обязан сверяться с бегущим максимумом концов предков.
			name: "пересечение предка сквозь вложенного отбрасывается",
			text: "абвгдеёжз",
			ents: []telego.MessageEntity{
				ent(telego.EntityTypeBold, 0, 6),
				ent(telego.EntityTypeItalic, 2, 2),
				ent(telego.EntityTypeUnderline, 5, 4),
			},
			want: "<b>аб<i>вг</i>де</b>ёжз",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := entitiesToHTML(tc.text, tc.ents)
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8 output: %q", got)
			}
			if got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestItalicInsideSurrogatePair — курсив, начинающийся ВНУТРИ surrogate
// pair: decode одинокой низкок surrogate даёт U+FFFD, но паники быть не
// должно, HTML — валидный.
func TestItalicInsideSurrogatePair(t *testing.T) {
	got := entitiesToHTML("😀ок", []telego.MessageEntity{ent(telego.EntityTypeItalic, 1, 3)})
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, "<i>") || !strings.Contains(got, "</i>") {
		t.Fatalf("expected italic tags, got %q", got)
	}
}

// TestReplyStoreReplaceCAS — семантика CAS-перехода стадии: живой pending
// атомарно перевзводится, устаревший указатель и уже изъятое ожидание — нет.
func TestReplyStoreReplaceCAS(t *testing.T) {
	s := newReplyStore()
	p := s.Put(1, 2, time.Now().Add(time.Minute), 0, 1)

	next, ok := s.Replace(1, 2, p, time.Now().Add(2*time.Minute), 0, 2)
	if !ok || next == nil || next.Stage != 2 {
		t.Fatalf("CAS on live pending must advance (ok=%v next=%v)", ok, next)
	}
	if cur, _ := s.Get(1, 2); cur != next {
		t.Fatal("store must hold the new pending after successful Replace")
	}

	if _, ok := s.Replace(1, 2, p, time.Now(), 0, 3); ok {
		t.Fatal("CAS against a stale pointer must lose")
	}

	if _, ok := s.Take(1, 2); !ok {
		t.Fatal("Take after Replace must return the new pending")
	}
	if _, ok := s.Replace(1, 2, next, time.Now(), 0, 3); ok {
		t.Fatal("CAS after Take must lose — ожидание уже разрешил другой")
	}
}

// pressSpamVote — общий клик «Да, спам» от имени voter по плашке botMsgID
// (раньше копировался в каждом sv:-тесте).
func pressSpamVote(t *testing.T, b *Bot, botMsgID, voter int64) {
	t.Helper()
	query := telego.CallbackQuery{
		ID: "q", From: telego.User{ID: voter, FirstName: "Голосующий"}, Data: "sv:1",
		Message: &telego.Message{MessageID: int(botMsgID),
			Chat: telego.Chat{ID: testChatID, Type: "supergroup"}},
	}
	if err := b.handleSpamVoteCallback(nil, query); err != nil {
		t.Fatal(err)
	}
}

// TestSpamVoteBallotTrustGateHandlerLevel — анти-sockpuppet защита именно на
// sv:-пути (раньше проверялась только на /spam-репорте): клики ниже порога
// доверия не попадают в tally и не банят; кворум добывают только доверенные;
// автор выше порога по-прежнему лишён голоса в своей плашке.
func TestSpamVoteBallotTrustGateHandlerLevel(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.PutSpamVote(ctx, storage.SpamVote{
		ChatID: testChatID, BotMsgID: 7, TargetMsgID: 555,
		AuthorID: 42, Prob: 100, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	ballots := func() int {
		t.Helper()
		yes, no, err := db.CountBallots(ctx, testChatID, 7)
		if err != nil {
			t.Fatal(err)
		}
		return yes + no
	}

	// (a) Несколько недоверенных кликов: бюллетеней нет, голосование живо,
	// наказаний нет — свежие куклы не продавливают вердикт.
	for voter := int64(201); voter <= 203; voter++ { // тотал 0 <= дефолтных 5
		pressSpamVote(t, b, 7, voter)
	}
	if n := ballots(); n != 0 {
		t.Fatalf("untrusted ballots recorded = %d, want 0", n)
	}
	if _, found, err := db.GetSpamVote(ctx, testChatID, 7); err != nil || !found {
		t.Fatalf("vote must survive untrusted clicks (found=%v err=%v)", found, err)
	}
	if fc.callCount("banChatMember") != 0 {
		t.Fatal("untrusted clicks must never punish")
	}

	// (b) Три доверенных «за» добирают дефолтную маржу 3 → ровно один бан.
	for voter := int64(101); voter <= 103; voter++ {
		for i := 0; i < 10; i++ {
			if _, err := db.RecordMessage(ctx, testChatID, voter, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	pressSpamVote(t, b, 7, 101)
	pressSpamVote(t, b, 7, 102)
	if n := ballots(); n != 2 {
		t.Fatalf("trusted ballots below margin = %d, want 2", n)
	}
	pressSpamVote(t, b, 7, 103)
	if fc.callCount("banChatMember") != 1 {
		t.Fatal("margin reached by trusted voters → ровно один banRevoke")
	}
	if banned, err := db.IsSpamBanned(ctx, 42); err != nil || !banned {
		t.Fatalf("IsSpamBanned = %v (err %v), want true", banned, err)
	}
}

// TestAuthorAboveThresholdExcludedFromOwnPlashka — автор, набравший порог
// сообщениями, всё равно не голосует в своей плашке (доверие ≠ право голоса
// за себя).
func TestAuthorAboveThresholdExcludedFromOwnPlashka(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.PutSpamVote(ctx, storage.SpamVote{
		ChatID: testChatID, BotMsgID: 9, TargetMsgID: 556,
		AuthorID: 42, Prob: 100, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ { // автор давно выше whitelist-порога
		if _, err := db.RecordMessage(ctx, testChatID, 42, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	pressSpamVote(t, b, 9, 42)
	yes, no, err := db.CountBallots(ctx, testChatID, 9)
	if err != nil {
		t.Fatal(err)
	}
	if yes+no != 0 {
		t.Fatalf("author ballot recorded (%d:%d), want none", yes, no)
	}
	if _, found, _ := db.GetSpamVote(ctx, testChatID, 9); !found {
		t.Fatal("author self-vote must not resolve the vote")
	}
}

// TestOnUserJoinedLateReplaySkipped — регресс дедупа ПОЗДНЕЙ дубль-доставки:
// Telegram переигрывает неподтверждённые апдейты после деплоя, и повтор может
// прийти уже после того, как капча разрешилась и UpsertMember записал свежий
// joined_at. Такой повтор обязан молча игнорироваться — без второй серии,
// рестрикта и второго join.
func TestOnUserJoinedLateReplaySkipped(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	user := telego.User{ID: testUserID, FirstName: "Вася"}

	// Свежий joined_at: юзер ТОЛЬКО ЧТО прошёл капчу.
	if err := db.UpsertMember(ctx, testChatID, user.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	b.onUserJoined(telego.Chat{ID: testChatID, Type: "supergroup"}, user, 0)

	// Синхронный выход: ни рестрикта, ни join-события, ни капчи в store.
	time.Sleep(100 * time.Millisecond) // окно, куда заехала бы goSafe-серия
	if n := fc.callCount("restrictChatMember"); n != 0 {
		t.Fatalf("replayed join must not re-restrict, restrict calls = %d", n)
	}
	if b.store.IsCaptchaActive(testChatID, user.ID) {
		t.Fatal("replayed join must not start a captcha")
	}
	s, err := db.QueryStats(ctx, testChatID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.Joined != 0 {
		t.Fatalf("join events = %d, want 0 (дубль не считается)", s.Joined)
	}

	// Контроль: устаревший joined_at — обычный вход, капча стартует.
	if err := db.UpsertMember(ctx, testChatID, user.ID, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	b.onUserJoined(telego.Chat{ID: testChatID, Type: "supergroup"}, user, 0)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && fc.callCount("restrictChatMember") == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := fc.callCount("restrictChatMember"); n == 0 {
		t.Fatal("stale joined_at must take the normal captcha path (restrict expected)")
	}
	for _, p := range b.store.TakeChat(testChatID) {
		p.Cancel()
	}
}
