package bot

import (
	"fmt"
	"html"
	"strings"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// pluralRU выбирает правильную русскую форму для count. Например: pluralRU(n, "день", "дня", "дней").
func pluralRU(n int, one, few, many string) string {
	mod100 := n % 100
	if mod100 >= 11 && mod100 <= 19 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	default:
		return many
	}
}

// minutesNom/minutesGen/minutesAcc — «N минут» в нужном падеже: формы
// «минута» склоняются, и один тапл на все контексты даёт «У тебя 1 минуту» /
// «в течение 2 минуты». Им. п. — «У тебя 2 минуты»; род. — «в течение 1
// минуты», «в течение 2 минут»; вин. (после «через») — «через 2 минуты».
func minutesNom(n int) string {
	return fmt.Sprintf("%d %s", n, pluralRU(n, "минута", "минуты", "минут"))
}

func minutesGen(n int) string {
	return fmt.Sprintf("%d %s", n, pluralRU(n, "минуты", "минут", "минут"))
}

func minutesAcc(n int) string {
	return fmt.Sprintf("%d %s", n, pluralRU(n, "минуту", "минуты", "минут"))
}

// humanDaysRU форматирует срок в днях как «N год/лет» / «N месяц/ев» / «N день/дней».
func humanDaysRU(days int) string {
	if days < 0 {
		days = 0
	}
	switch {
	case days >= 365:
		y := days / 365
		return fmt.Sprintf("%d %s", y, pluralRU(y, "год", "года", "лет"))
	case days >= 30:
		m := days / 30
		return fmt.Sprintf("%d %s", m, pluralRU(m, "месяц", "месяца", "месяцев"))
	default:
		return fmt.Sprintf("%d %s", days, pluralRU(days, "день", "дня", "дней"))
	}
}

// humanDaysGenRU — humanDaysRU в родительном падеже, для фраз «после N …»:
// «1 месяца», «2 месяцев», «1 года», «2 лет», «21 дня».
func humanDaysGenRU(days int) string {
	if days < 0 {
		days = 0
	}
	switch {
	case days >= 365:
		y := days / 365
		return fmt.Sprintf("%d %s", y, pluralRU(y, "года", "лет", "лет"))
	case days >= 30:
		m := days / 30
		return fmt.Sprintf("%d %s", m, pluralRU(m, "месяца", "месяцев", "месяцев"))
	default:
		return fmt.Sprintf("%d %s", days, pluralRU(days, "дня", "дней", "дней"))
	}
}

// mentionFromInfo рендерит кликабельный HTML-mention из закэшированной
// информации о юзере. Если имя неизвестно — фолбэк на id.
func mentionFromInfo(info storage.UserInfo) string {
	name := strings.TrimSpace(info.FirstName + " " + info.LastName)
	if name == "" && info.Username != "" {
		name = "@" + info.Username
	}
	if name == "" {
		name = fmt.Sprintf("id%d", info.UserID)
	}
	return fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, info.UserID, html.EscapeString(name))
}

// chatLinkHTML рендерит «Название» чата кликабельной HTML-ссылкой на сам чат:
// t.me/<username> для публичных, t.me/c/<id>/… для приватных супергрупп,
// плоский текст для обычных групп, у которых ссылки не существует.
// t.me/c-ссылка открывается только у участников чата; у владельца бота, не
// состоящего в чате, она мертва — принятый компромисс: рабочего формата
// ссылки для не-участника нет вовсе, а мёртвая не хуже плоского текста.
func chatLinkHTML(c storage.ChatInfo) string {
	title := "«" + html.EscapeString(titleOrID(c)) + "»"
	if c.Username != "" {
		return fmt.Sprintf(`<a href="https://t.me/%s">%s</a>`, c.Username, title)
	}
	if c.ChatID < -1_000_000_000_000 {
		// ponytail: огромный message_id — стандартный хак «открыть чат в конце»,
		// клиенты клампят его к последнему сообщению.
		return fmt.Sprintf(`<a href="https://t.me/c/%d/999999999">%s</a>`,
			-c.ChatID-1_000_000_000_000, title)
	}
	return title
}

// mentionOrID рендерит mention по данным из карты; если юзера там нет — id.
func mentionOrID(infos map[int64]storage.UserInfo, userID int64) string {
	if info, ok := infos[userID]; ok {
		return mentionFromInfo(info)
	}
	return fmt.Sprintf(`<a href="tg://user?id=%d">id%d</a>`, userID, userID)
}

// mentionWithUsername — mentionOrID плюс хвост « - @username», когда ник
// известен. Используется в списках статистики: ссылка tg://user молча
// вырождается в обычный текст у удалённых и закрытых приватностью аккаунтов,
// а литеральный @username остаётся кликабельным сам по себе. Хвоста нет,
// когда отображаемое имя И ЕСТЬ ник (нет имени/фамилии) — было бы дублем.
func mentionWithUsername(infos map[int64]storage.UserInfo, userID int64) string {
	m := mentionOrID(infos, userID)
	info, ok := infos[userID]
	if ok && info.Username != "" &&
		strings.TrimSpace(info.FirstName+" "+info.LastName) != "" {
		// Экранирование — hardening: сегодня username это [A-Za-z0-9_], но это
		// единственное пользовательское значение в ModeHTML без escape.
		m += " - @" + html.EscapeString(info.Username)
	}
	return m
}
