package bot

import (
	"fmt"
	"html"
	"strings"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// pluralRU picks the right Russian form for count. E.g. pluralRU(n, "день", "дня", "дней").
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

// humanDaysRU renders a duration given in days as "N год/лет" / "N месяц/ев" / "N день/дней".
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

// mentionFromInfo renders a clickable HTML mention using cached user info.
// Falls back to id if no name is known.
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

// mentionOrID renders a mention from a map lookup; if missing, shows the id.
func mentionOrID(infos map[int64]storage.UserInfo, userID int64) string {
	if info, ok := infos[userID]; ok {
		return mentionFromInfo(info)
	}
	return fmt.Sprintf(`<a href="tg://user?id=%d">id%d</a>`, userID, userID)
}
