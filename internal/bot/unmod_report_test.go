package bot

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	th "github.com/mymmrac/telego/telegohandler"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestHelpHTMLInGroupDMAndMenu(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	const user = int64(901)
	router, err := th.NewBotHandler(b.api, nil)
	if err != nil {
		t.Fatal(err)
	}
	router.HandleMessage(b.handleHelpCommand, th.CommandEqual("help"))
	router.HandleCallbackQuery(b.handleMenuCallback, th.CallbackDataPrefix("menu:"))
	group := privateMsg(user, "/help")
	group.Chat = telego.Chat{ID: testChatID, Type: telego.ChatTypeSupergroup}
	dm := privateMsg(user, "/help")
	for _, u := range []telego.Update{
		{Message: &group},
		{Message: &dm},
		{CallbackQuery: &telego.CallbackQuery{ID: "help", From: *dm.From, Message: &dm, Data: "menu:help"}},
	} {
		if err := router.BaseGroup().HandleUpdate(t.Context(), b.api, u); err != nil {
			t.Fatal(err)
		}
	}
	messages, edits := fc.callBodies("sendMessage"), fc.callBodies("editMessageText")
	if len(messages) != 2 || len(edits) != 1 {
		t.Fatalf("expected group, DM and menu help: %v", fc.callList())
	}
	for _, body := range append(messages, edits...) {
		var request struct {
			Text string `json:"text"`
			Mode string `json:"parse_mode"`
		}
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		if request.Mode != "HTML" || !strings.Contains(request.Text, "/replywithbot &lt;текст&gt;") {
			t.Fatalf("help lacks escaped command argument: %q", request.Text)
		}
		parser := xml.NewDecoder(strings.NewReader("<help>" + request.Text + "</help>"))
		for {
			token, err := parser.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("help markup failed: %v", err)
			}
			if tag, ok := token.(xml.StartElement); ok && tag.Name.Local != "help" && tag.Name.Local != "b" && tag.Name.Local != "i" {
				t.Fatalf("unexpected help tag: %s", tag.Name.Local)
			}
		}
	}
}

func TestFullunbanReportsAllChatsToOwner(t *testing.T) {
	b, db, fc := newFlowBot(t)
	ctx := b.runCtx
	const requester, target = 901, 555
	b.cfg.OwnerIDs = map[int64]struct{}{requester: {}}
	const success, rejected, hidden = -100201, -100202, -100203
	for i, chatID := range []int64{testChatID, success, rejected, hidden} {
		serviceableChat(t, b, db, chatID)
		names := []string{"Origin", "A Accepted", "B Declined", "C Private"}
		if err := db.RememberChat(ctx, storage.ChatInfo{ChatID: chatID, Title: names[i], Type: telego.ChatTypeSupergroup}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddSpamBanned(ctx, target, testChatID, time.Now()); err != nil {
		t.Fatal(err)
	}
	bot := `{"status":"administrator","user":{"id":42,"is_bot":true,"first_name":"Bot"},"can_restrict_members":true}`
	targetMember := `{"status":"member","user":{"id":555,"is_bot":false,"first_name":"Target"}}`
	fc.respSeq["getChatMember"] = []string{bot, targetMember}
	fc.errWhen = func(method string, data *telegoapi.RequestData) bool {
		return method == "unbanChatMember" && strings.Contains(string(data.BodyRaw), fmt.Sprintf(`"chat_id":%d`, rejected))
	}
	msg := reportCommand(requester, &telego.User{ID: target, FirstName: "Target"})
	msg.Text = "/fullunban"
	if err := b.handleFullunbanCommand(nil, msg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return fc.callCount("sendMessage") >= 2 })
	sent := fc.callBodies("sendMessage")
	if len(sent) != 2 || !strings.Contains(sent[0], "Разбан в этом чате подтверждён") ||
		!strings.Contains(sent[0], "Запись в базе спамеров удалена") || !strings.Contains(sent[0], "Запускаю обход") {
		t.Fatalf("missing separate local and queued outcomes: %v", sent)
	}
	if !strings.Contains(sent[1], `"chat_id":901`) || !strings.Contains(sent[1], "A Accepted» — разбан подтверждён") ||
		!strings.Contains(sent[1], "B Declined» — разбан не подтверждён") {
		t.Fatalf("private results did not distinguish outcomes: %v", sent)
	}
	if !strings.Contains(sent[1], "C Private") ||
		!strings.Contains(strings.Join(fc.callBodies("unbanChatMember"), ""), `"chat_id":-100203`) {
		t.Fatalf("owner did not receive the result for each chat: %v", sent)
	}
}

func TestLocalAdminCanOnlyUnbanInOwnChat(t *testing.T) {
	b, db, fc := newFlowBot(t)
	ctx := t.Context()
	const requester, target, otherChat = 200, 555, -100203
	b.cfg.OwnerIDs = map[int64]struct{}{901: {}}
	serviceableChat(t, b, db, testChatID)
	serviceableChat(t, b, db, otherChat)
	if err := db.AddSpamBanned(ctx, target, testChatID, time.Now()); err != nil {
		t.Fatal(err)
	}
	bot := `{"status":"administrator","user":{"id":42,"is_bot":true,"first_name":"Bot"},"can_restrict_members":true}`
	admin := `{"status":"administrator","user":{"id":200,"is_bot":false,"first_name":"Admin"}}`
	member := `{"status":"member","user":{"id":555,"is_bot":false,"first_name":"Target"}}`
	fc.respSeq["getChatMember"] = []string{bot, admin, admin, bot, admin, member}
	msg := reportCommand(requester, &telego.User{ID: target})
	msg.Text = "/fullunban"
	if err := b.handleFullunbanCommand(nil, msg); err != nil {
		t.Fatal(err)
	}
	query := telego.CallbackQuery{
		ID: "full", From: *msg.From, Data: "mc:f:555",
		Message: &telego.Message{MessageID: 42, Chat: msg.Chat},
	}
	if err := b.handleModChoiceCallback(nil, query); err != nil {
		t.Fatal(err)
	}
	if fc.callCount("unbanChatMember") != 0 {
		t.Fatalf("local admin ran a full unban: %v", fc.callList())
	}
	if got := strings.Join(fc.callBodies("answerCallbackQuery"), ""); !strings.Contains(got, "доступен владельцу") {
		t.Fatalf("old full-unban button did not explain rejection: %s", got)
	}
	msg.Text = "/unban"
	if err := b.handleUnbanCommand(nil, msg); err != nil {
		t.Fatal(err)
	}
	if calls := fc.callBodies("unbanChatMember"); len(calls) != 1 ||
		!strings.Contains(calls[0], fmt.Sprintf(`"chat_id":%d`, testChatID)) {
		t.Fatalf("local unban had an unexpected scope: %v", calls)
	}
	if banned, err := db.IsSpamBannedInChat(ctx, testChatID, target); err != nil || banned {
		t.Fatalf("local chat still applies the old spam ban: %v / %v", banned, err)
	}
	if banned, err := db.IsSpamBannedInChat(ctx, otherChat, target); err != nil || !banned {
		t.Fatalf("other chat lost protection from the existing spam verdict: %v / %v", banned, err)
	}
}
