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
		// Overflow-free форма: e.Offset+e.Length при Length у MaxInt64
		// переполнилась бы в отрицательное, кламп пропускал — и дальше по циклу
		// utf16.Decode падал бы slice-bounds panic. Гварды выше дают
		// n-e.Offset >= 1, так что вычитание безопасно.
		if e.Length > n-e.Offset {
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
	// Пересекающиеся (НЕ вложенные) entity отбрасываем: стек ниже умеет
	// закрывать только по LIFO, перекрёстная пара оставила бы незакрытый
	// тег и отказ отправки. Telegram такие не шлёт («либо вложены, либо не
	// пересекаются»), источник — только crafted greeting_entities.
	// Сравнение с БЕГУЩИМ МАКСИМУМОМ концов, а не только с последней
	// оставленной: тройка «предок — вложенный — пересекающий предка» иначе
	// просачивалась бы мимо парного фильтра (против вложенного соседа она
	// пересечением не выглядит).
	kept := ents[:0]
	maxEnd := 0
	for _, e := range ents {
		end := e.Offset + e.Length
		if e.Offset < maxEnd && end > maxEnd {
			continue
		}
		if end > maxEnd {
			maxEnd = end
		}
		kept = append(kept, e)
	}
	ents = kept

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
		return fmt.Sprintf(`<a href="%s">`, html.EscapeString(e.URL)) //nolint:gocritic // sprintfQuotedString: building HTML, not Go format
	}
	return "<" + htmlTagFor(e) + ">"
}

func closeTag(e telego.MessageEntity) string {
	if e.Type == telego.EntityTypeTextLink {
		return "</a>"
	}
	return "</" + htmlTagFor(e) + ">"
}
