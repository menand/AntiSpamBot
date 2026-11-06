package bot

import (
	"context"
	"fmt"
	"testing"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestModTargetUsernameRequiresCurrentMatch(t *testing.T) {
	for _, tc := range []struct {
		name, current string
		apiError      bool
		want          bool
	}{
		{name: "same", current: "SoMeOnE", want: true},
		{name: "renamed", current: "newname"},
		{name: "removed"},
		{name: "API failure", apiError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			if err := db.RememberUser(context.Background(), storage.UserInfo{UserID: testUserID, Username: "someone"}); err != nil {
				t.Fatal(err)
			}
			fc.resp["getChatMember"] = fmt.Sprintf(`{"status":"member","user":{"id":%d,"is_bot":false,"first_name":"User","username":%q}}`, testUserID, tc.current)
			if tc.apiError {
				fc.err["getChatMember"] = &telegoapi.Error{ErrorCode: 400, Description: "user unavailable"}
			}
			// Explicit @target must never fall through to an unrelated reply.
			id, _, ok := b.resolveModTarget(telego.Message{
				Chat: telego.Chat{ID: testChatID}, Text: "/ban @someone",
				ReplyToMessage: &telego.Message{From: &telego.User{ID: 99}},
			})
			if ok != tc.want || (ok && id != testUserID) {
				t.Fatalf("resolved id=%d ok=%v, want ok=%v", id, ok, tc.want)
			}
		})
	}
}
