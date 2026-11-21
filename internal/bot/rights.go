package bot

import (
	"context"
	"slices"
	"sync"
)

type rightsWaiter struct {
	ready chan struct{}
	mute  bool
}

type memberRightsQueue struct {
	mu    sync.Mutex
	lanes map[chatUser][]*rightsWaiter
}

// lock упорядочивает изменения прав одной пары чат-участник. Уже начатый
// запрос завершается раньше нового. Ожидающие ручные муты идут перед
// автоматикой, в порядке поступления.
func (q *memberRightsQueue) lock(ctx context.Context, chatID, userID int64, mute bool) (func(), error) {
	key := chatUser{chatID, userID}
	w := &rightsWaiter{ready: make(chan struct{}), mute: mute}
	q.mu.Lock()
	if q.lanes == nil {
		q.lanes = make(map[chatUser][]*rightsWaiter)
	}
	lane := q.lanes[key]
	if len(lane) == 0 {
		close(w.ready)
	}
	i := len(lane)
	if mute {
		for j := 1; j < len(lane); j++ {
			if !lane[j].mute {
				i = j
				break
			}
		}
	}
	q.lanes[key] = slices.Insert(lane, i, w)
	q.mu.Unlock()

	unlock := func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		lane := q.lanes[key]
		i := slices.Index(lane, w)
		lane = slices.Delete(lane, i, i+1)
		if len(lane) == 0 {
			delete(q.lanes, key)
			return
		}
		q.lanes[key] = lane
		if i == 0 {
			close(lane[0].ready)
		}
	}
	select {
	case <-ctx.Done():
	case <-w.ready:
		if ctx.Err() == nil {
			return unlock, nil
		}
	}
	unlock()
	return nil, ctx.Err()
}

// muteWaiting проверяется допуском после запроса к Telegram, прежде чем
// создавать ожидание ответа. Мут сам завершит сохранённое решение.
func (q *memberRightsQueue) muteWaiting(chatID, userID int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	lane := q.lanes[chatUser{chatID, userID}]
	if len(lane) < 2 {
		return false
	}
	return lane[1].mute
}
