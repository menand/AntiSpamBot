package bot

import (
	"testing"

	"github.com/mymmrac/telego"
)

func ent(typ string, offset, length int) telego.MessageEntity {
	return telego.MessageEntity{Type: typ, Offset: offset, Length: length}
}

func TestEntitiesToHTML(t *testing.T) {
	tests := []struct {
		name string
		text string
		ents []telego.MessageEntity
		want string
	}{
		{
			name: "без entities — просто экранирование",
			text: `привет <b> & "мир"`,
			ents: nil,
			want: `привет &lt;b&gt; &amp; &#34;мир&#34;`,
		},
		{
			name: "жирный посреди кириллицы",
			text: "Привет, добрый чат!",
			ents: []telego.MessageEntity{ent(telego.EntityTypeBold, 8, 6)},
			want: "Привет, <b>добрый</b> чат!",
		},
		{
			name: "жирный и курсив рядом",
			text: "раз два три",
			ents: []telego.MessageEntity{
				ent(telego.EntityTypeBold, 0, 3),
				ent(telego.EntityTypeItalic, 4, 3),
			},
			want: "<b>раз</b> <i>два</i> три",
		},
		{
			name: "вложенность: курсив внутри жирного",
			text: "весь мир театр",
			ents: []telego.MessageEntity{
				ent(telego.EntityTypeBold, 0, 8),
				ent(telego.EntityTypeItalic, 5, 3),
			},
			want: "<b>весь <i>мир</i></b> театр",
		},
		{
			// Эмодзи 😀 = 2 UTF-16 юнита: офсеты после неё сдвинуты. Bold
			// покрывает «жир» — офсет 5 (2 юнита эмодзи + пробел + «а» — нет:
			// текст "😀 ажир": 😀=2, пробел=1, а=1 → «жир» с юнита 4.
			name: "UTF-16: эмодзи перед выделением",
			text: "😀 ажир",
			ents: []telego.MessageEntity{ent(telego.EntityTypeBold, 4, 3)},
			want: "😀 а<b>жир</b>",
		},
		{
			name: "экранирование внутри тега",
			text: "смотри <тут>",
			ents: []telego.MessageEntity{ent(telego.EntityTypeBold, 7, 5)},
			want: "смотри <b>&lt;тут&gt;</b>",
		},
		{
			name: "text_link с кавычкой в URL",
			text: "ссылка",
			ents: []telego.MessageEntity{{
				Type: telego.EntityTypeTextLink, Offset: 0, Length: 6,
				URL: `https://e.com/?q="x"`,
			}},
			want: `<a href="https://e.com/?q=&#34;x&#34;">ссылка</a>`,
		},
		{
			name: "неформатирующие типы игнорируются",
			text: "@user привет",
			ents: []telego.MessageEntity{ent(telego.EntityTypeMention, 0, 5)},
			want: "@user привет",
		},
		{
			name: "выход за границы клампится",
			text: "аб",
			ents: []telego.MessageEntity{ent(telego.EntityTypeBold, 1, 99)},
			want: "а<b>б</b>",
		},
		{
			name: "спойлер и зачёркнутый",
			text: "секрет и старое",
			ents: []telego.MessageEntity{
				ent(telego.EntityTypeSpoiler, 0, 6),
				ent(telego.EntityTypeStrikethrough, 9, 6),
			},
			want: "<tg-spoiler>секрет</tg-spoiler> и <s>старое</s>",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := entitiesToHTML(tc.text, tc.ents); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
