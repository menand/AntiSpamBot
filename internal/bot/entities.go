package bot

import (
	"fmt"
	"html"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/mymmrac/telego"
)

// entitiesToHTML конвертирует текст с Telegram-entities (жирный, курсив,
// ссылки...) в HTML для отправки с ParseMode HTML. Offset/Length у entities
// Bot API считает в UTF-16 code units — текст сначала перегоняется в UTF-16,
// иначе любая эмодзи или кириллица до выделения сдвинула бы границы.
// Пользовательский текст экранируется здесь же (результат готов к отправке
// без повторного EscapeString). Telegram гарантирует, что entities либо
// вложены, либо не пересекаются — вложенность обходится стеком: вершина
// стека всегда закрывается первой (её конец минимален среди открытых).
// Неизвестные типы (mention, hashtag, url...) выводятся как обычный текст.
func entitiesToHTML(text string, entities []telego.MessageEntity) string {
	units := utf16.Encode([]rune(text))
	n := len(units)

	// Оставляем только поддерживаемые entities с валидными границами.
	ents := make([]telego.MessageEntity, 0, len(entities))
	for _, e := range entities {
		if htmlTagFor(e) == "" && e.Type != telego.EntityTypeTextLink {
			continue
		}
		if e.Offset < 0 || e.Length <= 0 || e.Offset >= n {
			continue
		}
		if e.Offset+e.Length > n {
			e.Length = n - e.Offset
		}
		ents = append(ents, e)
	}
	if len(ents) == 0 {
		return html.EscapeString(text)
	}
	// Внешние entities раньше внутренних: offset по возрастанию, при равных —
	// длиннее первой.
	sort.SliceStable(ents, func(i, j int) bool {
		if ents[i].Offset != ents[j].Offset {
			return ents[i].Offset < ents[j].Offset
		}
		return ents[i].Length > ents[j].Length
	})

	type open struct {
		ent telego.MessageEntity
		end int
	}
	var sb strings.Builder
	var stack []open
	ei, pos := 0, 0
	flush := func(from, to int) {
		if from < to {
			sb.WriteString(html.EscapeString(string(utf16.Decode(units[from:to]))))
		}
	}
	for {
		for len(stack) > 0 && stack[len(stack)-1].end == pos {
			sb.WriteString(closeTag(stack[len(stack)-1].ent))
			stack = stack[:len(stack)-1]
		}
		for ei < len(ents) && ents[ei].Offset == pos {
			sb.WriteString(openTag(ents[ei]))
			stack = append(stack, open{ent: ents[ei], end: ents[ei].Offset + ents[ei].Length})
			ei++
		}
		if pos >= n {
			break
		}
		next := n
		if len(stack) > 0 && stack[len(stack)-1].end < next {
			next = stack[len(stack)-1].end
		}
		if ei < len(ents) && ents[ei].Offset < next {
			next = ents[ei].Offset
		}
		flush(pos, next)
		pos = next
	}
	return sb.String()
}

// htmlTagFor — имя HTML-тега для типа entity; "" = тип не форматирующий
// (text_link обрабатывается отдельно — ему нужен href).
func htmlTagFor(e telego.MessageEntity) string {
	switch e.Type {
	case telego.EntityTypeBold:
		return "b"
	case telego.EntityTypeItalic:
		return "i"
	case telego.EntityTypeUnderline:
		return "u"
	case telego.EntityTypeStrikethrough:
		return "s"
	case telego.EntityTypeSpoiler:
		return "tg-spoiler"
	case telego.EntityTypeCode:
		return "code"
	case telego.EntityTypePre:
		return "pre"
	case telego.EntityTypeBlockquote, telego.EntityTypeExpandableBlockquote:
		return "blockquote"
	}
	return ""
}

func openTag(e telego.MessageEntity) string {
	if e.Type == telego.EntityTypeTextLink {
		return fmt.Sprintf(`<a href="%s">`, html.EscapeString(e.URL))
	}
	return "<" + htmlTagFor(e) + ">"
}

func closeTag(e telego.MessageEntity) string {
	if e.Type == telego.EntityTypeTextLink {
		return "</a>"
	}
	return "</" + htmlTagFor(e) + ">"
}
