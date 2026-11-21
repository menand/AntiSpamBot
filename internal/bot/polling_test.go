package bot

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	th "github.com/mymmrac/telego/telegohandler"
)

func TestPollingWaitsForHandlersAndSkipsSavedUpdates(t *testing.T) {
	b, db, fc := newFlowBot(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.MarkTelegramUpdateHandled(ctx, b.me.ID, 41); err != nil {
		t.Fatal(err)
	}
	fc.errSeq["getUpdates"] = []*telegoapi.Error{{ErrorCode: 409}, nil}
	fc.respSeq["getUpdates"] = []string{`[]`, `[{"update_id":41},{"update_id":42}]`, `[]`}
	requests := make(chan telego.GetUpdatesParams, 4)
	var requestsMade int
	fc.errWhen = func(method string, data *telegoapi.RequestData) bool {
		if method == "getUpdates" {
			requestsMade++
			var params telego.GetUpdatesParams
			if err := json.Unmarshal(data.BodyRaw, &params); err != nil {
				t.Error(err)
			}
			requests <- params
			if requestsMade == 4 {
				cancel()
			}
		}
		return false
	}
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var successes atomic.Int32
	routes := &th.HandlerGroup{}
	routes.Handle(func(_ *th.Context, update telego.Update) error {
		if update.UpdateID != 42 {
			t.Errorf("already completed update replayed: %d", update.UpdateID)
			return nil
		}
		close(entered)
		<-release
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- b.pollUpdates(ctx, routes, func() { successes.Add(1) }) }()
	read := func() telego.GetUpdatesParams {
		t.Helper()
		select {
		case p := <-requests:
			return p
		case <-ctx.Done():
			t.Fatal("getUpdates request timed out")
			return telego.GetUpdatesParams{}
		}
	}
	if first, second := read(), read(); first.Timeout <= 0 || first.Offset != 0 || second.Offset != 0 {
		t.Fatalf("wrong request after retry: %+v / %+v", first, second)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler did not receive update")
	}
	if n := successes.Load(); n != 1 {
		t.Fatalf("a rejected poll counted as ready: %d successful polls", n)
	}
	select {
	case p := <-requests:
		t.Fatalf("unfinished update was confirmed: %+v", p)
	default:
	}
	unblock()
	if ack, idle := read(), read(); ack.Offset != 43 || idle.Offset != 0 {
		t.Fatalf("wrong confirmation or idle offset: %+v / %+v", ack, idle)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if saved, err := db.HandledTelegramUpdates(t.Context(), b.me.ID); err != nil || len(saved) != 0 {
		t.Fatalf("confirmed update IDs were retained: %v / %v", saved, err)
	}
}
