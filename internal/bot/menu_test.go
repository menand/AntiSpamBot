package bot

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mymmrac/telego/telegoapi"
)

func TestIsNotModified(t *testing.T) {
	if isNotModified(nil) {
		t.Fatal("nil is not a not-modified error")
	}
	if isNotModified(errors.New("Bad Request: message is not modified")) {
		t.Fatal("plain errors must not match — only telegoapi.Error")
	}
	notMod := &telegoapi.Error{
		ErrorCode:   400,
		Description: "Bad Request: message is not modified: specified new message content and reply markup are exactly the same",
	}
	if !isNotModified(notMod) {
		t.Fatal("telegram not-modified error must match")
	}
	if !isNotModified(fmt.Errorf("edit: %w", notMod)) {
		t.Fatal("wrapped not-modified error must match")
	}
	if isNotModified(&telegoapi.Error{ErrorCode: 400, Description: "Bad Request: chat not found"}) {
		t.Fatal("other API errors must not match")
	}
}
