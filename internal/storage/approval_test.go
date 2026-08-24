package storage

import (
	"context"
	"sync"
	"testing"
)

func TestChatApprovalLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	// Новая строка реестра по умолчанию approved: чаты, существовавшие до
	// введения подтверждения (и любые, зарегистрированные без явного статуса),
	// должны работать как раньше.
	if err := db.RememberChat(ctx, ChatInfo{ChatID: 1, Title: "A", Type: "group"}); err != nil {
		t.Fatal(err)
	}
	status, exists, err := db.GetChatApproval(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || status != ChatApproved {
		t.Fatalf("default approval = (%q, %v), want (%q, true)", status, exists, ChatApproved)
	}

	// Неизвестный чат — exists=false, статуса нет.
	if _, exists, _ := db.GetChatApproval(ctx, 999); exists {
		t.Fatal("unknown chat must not exist in the registry")
	}

	// Переходы pending → approved → rejected читаются обратно как есть.
	for _, want := range []string{ChatPending, ChatApproved, ChatRejected} {
		if err := db.SetChatApproval(ctx, 1, want); err != nil {
			t.Fatalf("set %s: %v", want, err)
		}
		got, exists, err := db.GetChatApproval(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !exists || got != want {
			t.Fatalf("after set %s got (%q, %v)", want, got, exists)
		}
	}

	// Upsert создаёт строку для чата, которого ещё нет (путь pending в
	// handleMyChatMember: строка появляется до rememberChat).
	if err := db.SetChatApproval(ctx, 2, ChatPending); err != nil {
		t.Fatal(err)
	}
	if got, exists, _ := db.GetChatApproval(ctx, 2); !exists || got != ChatPending {
		t.Fatalf("upsert-created row = (%q, %v), want (pending, true)", got, exists)
	}

	// Невалидный статус — ошибка, статус не меняется.
	if err := db.SetChatApproval(ctx, 1, "bogus"); err == nil {
		t.Fatal("invalid status must error")
	}
	if got, _, _ := db.GetChatApproval(ctx, 1); got != ChatRejected {
		t.Fatalf("status after invalid write = %q, want %q", got, ChatRejected)
	}

	// Удаление строки реестра (dropChat) уносит и статус: повторное добавление
	// такого чата снова выглядит новым.
	if err := db.DeleteChat(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, exists, _ := db.GetChatApproval(ctx, 1); exists {
		t.Fatal("chat deleted from registry must lose its approval row")
	}
}

func TestClaimChatApproval(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	// Первый претендент переводит pending → approved и выигрывает.
	_ = db.SetChatApproval(ctx, 1, ChatPending)
	claimed, err := db.ClaimChatApproval(ctx, 1, ChatApproved)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("first claim from pending must win")
	}
	if got, _, _ := db.GetChatApproval(ctx, 1); got != ChatApproved {
		t.Fatalf("after claim status = %q, want approved", got)
	}

	// Второй претендент (гонка «Нет» против победившего «Да») проигрывает:
	// статус уже не pending, переход не сделан.
	claimed, err = db.ClaimChatApproval(ctx, 1, ChatRejected)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("second claim must lose when status is no longer pending")
	}
	if got, _, _ := db.GetChatApproval(ctx, 1); got != ChatApproved {
		t.Fatalf("losing claim must not change status, got %q", got)
	}

	// Claim по чату, которого нет в реестре, — false без ошибки.
	claimed, err = db.ClaimChatApproval(ctx, 999, ChatApproved)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("claim on unknown chat must not claim")
	}
}

// TestClaimChatApprovalConcurrentFirstPressWins — контракт атомарности под
// настоящей конкуренцией: telego обрабатывает callback'и параллельно, и
// несколько владельцев могут нажать «Да»/«Нет» одновременно. Ровно один
// претендент выигрывает, финальный статус совпадает с решением победителя —
// никогда не смесь.
func TestClaimChatApprovalConcurrentFirstPressWins(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		first string // решение горутин при same=true; иначе — половины
		same  bool   // все претенденты требуют одно и то же решение
	}{
		{name: "approved против rejected", first: ChatApproved},
		{name: "rejected против approved", first: ChatRejected},
		{name: "одинаковые решения", first: ChatApproved, same: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTest(t)
			if err := db.SetChatApproval(ctx, 1, ChatPending); err != nil {
				t.Fatal(err)
			}

			opposite := ChatRejected
			if tc.first == ChatRejected {
				opposite = ChatApproved
			}
			const racers = 8
			var (
				wg        sync.WaitGroup
				wins      = make([]bool, racers)
				decisions = make([]string, racers)
			)
			for i := 0; i < racers; i++ {
				d := opposite
				if tc.same || i%2 == 0 {
					d = tc.first
				}
				decisions[i] = d
				wg.Add(1)
				go func(i int, d string) {
					defer wg.Done()
					claimed, err := db.ClaimChatApproval(ctx, 1, d)
					if err != nil {
						t.Errorf("claim %q: %v", d, err)
						return
					}
					wins[i] = claimed
				}(i, d)
			}
			wg.Wait()

			total := 0
			wantStatus := ""
			for i, w := range wins {
				if w {
					total++
					wantStatus = decisions[i]
				}
			}
			if total != 1 {
				t.Fatalf("claims won = %d, want exactly 1 (%v)", total, wins)
			}
			got, exists, err := db.GetChatApproval(ctx, 1)
			if err != nil || !exists {
				t.Fatalf("get after race: (%q, %v, err %v)", got, exists, err)
			}
			if got != wantStatus {
				t.Fatalf("final status = %q, want winner's decision %q (никаких смесей)", got, wantStatus)
			}
		})
	}

	t.Run("неизвестный чат — все проигрывают", func(t *testing.T) {
		db := openTest(t)
		var inner sync.WaitGroup
		results := make([]bool, 4)
		for i := 0; i < 4; i++ {
			inner.Add(1)
			go func(i int) {
				defer inner.Done()
				claimed, err := db.ClaimChatApproval(ctx, 424242, ChatApproved)
				if err != nil {
					t.Error(err)
				}
				results[i] = claimed
			}(i)
		}
		inner.Wait()
		for _, claimed := range results {
			if claimed {
				t.Fatal("claim on unknown chat must not win")
			}
		}
	})
}

func TestReapproveChat(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	// Путь восстановления: чат был отклонён, но выход бота не прошёл (строка
	// 'rejected' ещё существует) — повторный апрув переводит обратно в approved.
	_ = db.SetChatApproval(ctx, 1, ChatRejected)
	reapproved, err := db.ReapproveChat(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reapproved {
		t.Fatal("reapprove of an existing rejected chat must succeed")
	}
	if got, _, _ := db.GetChatApproval(ctx, 1); got != ChatApproved {
		t.Fatalf("after reapprove status = %q, want approved", got)
	}

	// Гонка с dropChat: строка уже удалена (бот вышел) — ReapproveChat обязан
	// вернуть false и НЕ создавать строку заново (воскрешение мёртвого чата).
	_ = db.SetChatApproval(ctx, 2, ChatRejected)
	if err := db.DeleteChat(ctx, 2); err != nil {
		t.Fatal(err)
	}
	reapproved, err = db.ReapproveChat(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if reapproved {
		t.Fatal("reapprove must not resurrect a dropped chat")
	}
	if _, exists, _ := db.GetChatApproval(ctx, 2); exists {
		t.Fatal("reapprove must not create a registry row for a dropped chat")
	}

	// Повторный reapprove уже approved-чата — false без ошибки (идемпотентность).
	reapproved, err = db.ReapproveChat(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if reapproved {
		t.Fatal("reapprove of an approved chat must not claim")
	}

	// Reapprove по чату, которого вообще нет, — false без ошибки.
	reapproved, err = db.ReapproveChat(ctx, 999)
	if err != nil {
		t.Fatal(err)
	}
	if reapproved {
		t.Fatal("reapprove of unknown chat must not claim")
	}
}
