// btnprobe — отправляет тестовую клавиатуру в ЛС, чтобы вживую смотреть, как
// клиент Telegram обрезает подписи кнопок.
//
// Зачем: лимит кнопки — это визуальная ШИРИНА, а не руны. Сколько букв
// влезет, зависит от шрифта конкретного клиента (iOS/Android/Desktop), и
// из Go пиксели не измерить. Поэтому подбор лимита — ручной: шлём кнопки
// разной ширины и смотрим, где клиент сам ставит «…».
//
// Запуск: токен берётся из переменной окружения BOT_TOKEN и никуда не
// печатается — подставляйте его из своего .env, сами доступы в репозиторий
// не зашиваются.
//
//	BOT_TOKEN=... go run ./cmd/btnprobe -chat <CHAT_ID> -mode mixed
//
// Флаги:
//
//	-chat N    — куда слать (обязателен)
//	-mode M    — mixed|width|emoji|rows|edge|pair|probe|tri|seq|eseq|kseq|settings (дефолт mixed)
//	-budget N  — бюджет ширины для обрезки (дефолт — textwidth.MaxLabelWidth,
//	             то есть ровно то, что стоит в боте)
//	-rows N    — сколько кнопок в ряду в режиме rows (клампится 2..6)
//	-rowsize N — кнопок в ряду в режиме width (дефолт 1)
//	-from N    — первая ширина в режиме edge (руны, дефолт 17)
//	-to N      — последняя ширина в режиме edge (руны, дефолт 24)
//	-per N     — кнопок в ряду в режиме edge (дефолт 2)
//	-widths S  — разбивка в режиме pair: ряды через «;», кнопки через «,»
//
// Режимы:
//
//	mixed  — обзорный набор: обрезка, эмодзи, чередование
//	width  — подписи шириной вокруг бюджета
//	emoji  — эмодзи в разных позициях, проверка кластеров
//	rows   — ряды из N одинаковых подписей разной ширины
//	edge   — КЛИЕНТСКАЯ граница: подписи заданной ширины, решение за клиентом
//	pair   — ряды из 2 кнопок с заданной разбивкой ширины
//	probe  — ряды 2/3/4/5 вокруг измеренного эталона ±2/±5/±10
//	tri    — ряды из 3 кнопок: max−1 / max / max+1 для лимитов 11, 12, 13
//	settings — превью экрана «Настройки чата» (ряды как в боте); -state on|off
//	           решает, показывать ли пресеты ИИ-антиспама и карантина
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/menand/AntiSpamBot/internal/textwidth"
	"github.com/mymmrac/telego"
)

// label — подпись кнопки в тестовой клавиатуре.
type label struct {
	text string
}

func main() {
	var (
		chatID   = flag.Int64("chat", 0, "куда слать (обязателен)")
		mode     = flag.String("mode", "mixed", "mixed|width|emoji|rows|edge|pair|probe|tri|seq|eseq|kseq|settings")
		budget   = flag.Int("budget", textwidthBudgetDefault(), "бюджет ширины для обрезки")
		rowsN    = flag.Int("rows", 4, "кнопок в ряду в режиме rows (2..5)")
		rowSize  = flag.Int("rowsize", 1, "кнопок в ряду в режиме width")
		edgeFrom = flag.Int("from", 17, "первая ширина в режиме edge (руны)")
		edgeTo   = flag.Int("to", 24, "последняя ширина в режиме edge (руны)")
		perRow   = flag.Int("per", 2, "кнопок в ряду в режиме edge")
		sizes    = flag.String("sizes", "1,2,3,4,5", "какие размеры ряда слать в режиме seq: список через запятую")
		limit    = flag.Int("limit", 0, "перебить лимит в режиме seq (0 = брать из textwidth.LabelBudget)")
		widths   = flag.String("widths", "15,23;17,21;19,19;21,17;23,15", "ширины кнопок в режиме pair: ряды через ;, кнопки через ,")
		state    = flag.String("state", "on", "переключатели в режиме settings: on|off")
	)
	flag.Parse()

	if *chatID <= 0 {
		fmt.Fprintln(os.Stderr, "usage: btnprobe -chat N [-mode mixed|width|emoji|rows|edge|pair|probe|tri] [-budget 30] [-rows 4] [-rowsize 1] [-from 17] [-to 24] [-per 2] [-widths '15,23;17,21']")
		os.Exit(2)
	}
	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		fatal("BOT_TOKEN is empty", nil)
	}

	kb := buildKeyboard(*mode, *budget, *rowsN, *rowSize, *sizes, *limit, *widths, *edgeFrom, *edgeTo, *perRow, *state)

	// Печатаем, что именно ушло: исходная строка и её обрезанная версия.
	// Без этого непонятно, какая кнопка из какого источника родилась.
	for _, row := range kb {
		var parts []string
		for _, b := range row {
			parts = append(parts, fmt.Sprintf("%q(=%d)", b.Text, textwidth.Width(b.Text)))
		}
		fmt.Println(strings.Join(parts, " | "))
	}

	api, err := telego.NewBot(token)
	if err != nil {
		fatal("new bot", err)
	}
	// ctx.Background обязателен: telego передаёт его в HTTP-вызов, и nil
	// роняет процесс паникой в FastHTTPCaller.Call.
	msg, err := api.SendMessage(context.Background(), &telego.SendMessageParams{
		ChatID:    telego.ChatID{ID: *chatID},
		Text:      header(*mode, *state, *budget, *rowsN, *rowSize, *perRow, *edgeFrom, *edgeTo),
		ParseMode: telego.ModeHTML,
		ReplyMarkup: &telego.InlineKeyboardMarkup{
			InlineKeyboard: kb,
		},
	})
	if err != nil {
		fatal("send", err)
	}
	fmt.Printf("sent: chat=%d message_id=%d rows=%d\n", *chatID, msg.MessageID, len(kb))
}

// textwidthBudgetDefault — дефолт равен константе бота MaxLabelWidth, чтобы
// утилита по умолчанию показывала ровно то, что увидит юзер в меню.
func textwidthBudgetDefault() int { return textwidth.MaxLabelWidth }

func header(mode, state string, budget, rowsN, rowSize, perRow, edgeFrom, edgeTo int) string {
	switch mode {
	case "settings":
		if state == "off" {
			return "⚙️ <b>Превью: Настройки чата</b>\nИИ-антиспам и карантин ВЫКЛЮЧЕНЫ — их пресеты скрыты.\nЭто превью: кнопки кликабельны, но бот их не обрабатывает."
		}
		return "⚙️ <b>Превью: Настройки чата</b>\nИИ-антиспам и карантин ВКЛЮЧЕНЫ — пресеты видны прямо под кнопками.\nЭто превью: кнопки кликабельны, но бот их не обрабатывает."
	case "kseq":
		return "🧪 <b>Клавиши-эмодзи на максимум</b>\nРяды 1–5 кнопок, подпись = 0️⃣1️⃣2️⃣… ровно в лимит (33/17/10/7/5).\nКлавиша считается за 2 — так её рисует клиент.\nСмотри, совпадает ли граница с той же, что на буквах."
	case "eseq":
		return "🧪 <b>Эмодзи вместо букв</b>\nРяды 1–5 кнопок, лимиты 33/17/11/7/6 ±1.\nПодпись = всё 🔥 кроме хвоста из букв (16×🔥 + 1 буква = 33).\nСмотри, считает ли клиент смайлики за 2 и где режет."
	case "seq":
		return "🧪 <b>Контрольные замеры, 4 и 5 кнопок в ряду</b>\nРяды из 4 кнопок: ширина 6 / 7 / 8.\nРяды из 5 кнопок: ширина 4 / 5 / 6.\nПодпись — последовательность «абвгде…я1234567890».\nСмотри, на какой ширине клиент добавляет своё многоточие."
	case "tri":
		return "🧪 <b>Ряд из 3 кнопок, граница лимита</b>\nВ каждом ряду: ширина max−1 / max / max+1 для лимитов 11, 12 и 13.\nСмотри, в каком ряду третья кнопка начинает показывать многоточие клиента."
	case "edge":
		return fmt.Sprintf("🧪 <b>Клиентский лимит, %d кнопки в ряду</b>\nПодписи шириной %d…%d, обрезка наша по бюджету %d.\nСмотри, где клиент ДОБАВЛЯЕТ своё многоточие поверх нашего.", perRow, edgeFrom, edgeTo, budget)
	case "pair":
		return "🧪 <b>Ряд из 2 кнопок, разбивка ширины</b>\nСумма 38, но делится по-разному: 15+23 / 17+21 / 19+19 / 21+17 / 23+15.\nСмотри, в какой разбивке клиент начинает резать вторую кнопку."
	case "probe":
		return "🧪 <b>Клиентский лимит по размеру ряда</b>\nРяды 2/3/4/5 кнопок вокруг измеренного эталона ±2/±5/±10.\nСмотри, в каком ряду и на каком смещении клиент начинает ставить «…»."
	case "width":
		return fmt.Sprintf("🧪 <b>Ширина подписи</b> (бюджет %d, в ряду %d). Сверху вниз ширина растёт: смотри, где клиент сам ставит «…».", budget, rowSize)
	case "emoji":
		return fmt.Sprintf("🧪 <b>Эмодзи и ширина</b> (бюджет %d). Смотри, не режутся ли кластеры и что влезает.", budget)
	case "rows":
		return fmt.Sprintf("🧪 <b>Ряд из %d кнопок</b> (бюджет %d). Проверяю перенос и обрезку в узком ряду.", rowsN, budget)
	default:
		return fmt.Sprintf("🧪 <b>Смешанный тест</b> (бюджет %d). Одни эмодзи / в конце / чередование / ряды.", budget)
	}
}

// probe — ряд из n кнопок вокруг эталонной подписи base: сама base, плюс
// варианты на ±2, ±5, ±10 единиц ширины. Задаётся в рунах, потому что
// базовые подписи — чистая кириллица, где руна = 1 единица ширины.
type probe struct {
	n    int
	base string
}

// probes — базовые подписи, снятые вживую на клиенте (Telegram Desktop,
// октябрь 2026, чистая кириллица) возле границы ряда из n кнопок. Клиентский
// лимит зависит от шрифта и отличается в iOS/Android/Desktop, поэтому здесь
// лежат измеренные значения, а не константы бота.
//
// Значения подобраны так, чтобы база лежала в районе границы: ширины сейчас
// 20/12/7/6 при бюджетах 17/10/7/5. Это НЕ бюджеты кода — они живут в
// textwidth.LabelBudget, и обрезку бот делает по ним. Здесь важно только,
// чтобы смещения probeOffsets (±2/±5/±10) гарантированно перекрывали момент,
// когда клиент начинает ставить «…».
//
// Если после перезамера клиентский лимит сдвинется — правится ТОЛЬКО эта
// таблица, константы бота не трогаются.
var probes = []probe{
	{2, "Беговой чат жителей"},
	{3, "Чат жителей"},
	{4, "Наш дво"},
	{5, "Наш д"},
}

// probeOffsets — смещения вокруг эталона. ±2 = вблизи границы, ±5/±10 =
// заметно уже/дальше, чтобы увидеть, где клиент начинает резать.
var probeOffsets = []int{-10, -5, -2, 0, 2, 5, 10}

// probeRows строит по одному ряду на каждый размер: n кнопок одной подписи,
// ширина которой смещена относительно эталона.
func probeRows() [][]telego.InlineKeyboardButton {
	var kb [][]telego.InlineKeyboardButton
	for _, p := range probes {
		for _, off := range probeOffsets {
			w := textwidth.Width(p.base) + off
			if w < 1 {
				continue
			}
			row := make([]telego.InlineKeyboardButton, 0, p.n)
			for i := 0; i < p.n; i++ {
				row = append(row, telego.InlineKeyboardButton{
					Text:         labelAtWidth(p.base, w),
					CallbackData: "probe",
				})
			}
			kb = append(kb, row)
		}
	}
	return kb
}

// labelAtWidth обрезает или ДОПОЛНЯЕТ подпись до w единиц ширины. Дополнение —
// реальными буквами названия, а не точками: точки другой ширины и искажают
// границу, ради которой всё и меряется.
func labelAtWidth(base string, w int) string {
	if textwidth.Width(base) >= w {
		return textwidth.Truncate(base, w)
	}
	// Наращиваем названием-донором: «Чат жителей» + « Перово» и т.п.
	donor := base
	for textwidth.Width(donor) < w {
		donor += " Перово"
	}
	return textwidth.Truncate(donor, w)
}

// labelExact даёт подпись ровно w единиц ширины БЕЗ многоточия.
//
// Нужен для проверки клиентского лимита: мы не обрезаем сами, а кладём строку
// заданной ширины целиком и смотрим, поставит ли «…» клиент. Если бы
// обрезали тут — тест мерил бы нашу обрезку, а не клиента.
func labelExact(base string, w int) string {
	if textwidth.Width(base) >= w {
		return textwidth.Exact(base, w)
	}
	donor := base
	for textwidth.Width(donor) < w {
		donor += " Перово"
	}
	return textwidth.Exact(donor, w)
}

// edgeRows — ряды из двух КОНЕЧНЫХ подписей разной ширины, без нашей обрезки.
//
// Диапазон задаётся рунами (чистая кириллица, где руна = 1 единица ширины).
// Обе кнопки ряда одинаковой ширины: так видно, на каком значении клиент
// начинает резать при двух кнопках в ряду.
func edgeRows(from, to, perRow, budget int) [][]telego.InlineKeyboardButton {
	base := "Беговой чат жителей Измайловского района"
	var kb [][]telego.InlineKeyboardButton
	var row []telego.InlineKeyboardButton
	for w := from; w <= to; w++ {
		// Строка шириной w — исходник; обрезка ровно та, что уйдёт юзеру.
		row = append(row, telego.InlineKeyboardButton{
			Text:         textwidth.Truncate(labelExact(base, w), budget),
			CallbackData: "probe",
		})
		if len(row) == perRow {
			kb = append(kb, row)
			row = nil
		}
	}
	if len(row) > 0 {
		kb = append(kb, row)
	}
	return kb
}

// pairRows строит ряды из двух кнопок с заданными ширины.
//
// spec — «15,23;17,21;19,19», то есть ряды через «;», ширины кнопок через «,».
// В каждой паре сумма одинакова (38), но разбивка разная: так видно, как
// клиент делит ширину ряда между двумя кнопками.
//
// Ширина задаётся в рунах, а не в единицах textwidth: подписи — чистая
// кириллица, где руна = 1 единица, а так spec читается человеком.
func pairRows(spec string) [][]telego.InlineKeyboardButton {
	var kb [][]telego.InlineKeyboardButton
	for _, rowSpec := range strings.Split(spec, ";") {
		rowSpec = strings.TrimSpace(rowSpec)
		if rowSpec == "" {
			continue
		}
		var row []telego.InlineKeyboardButton
		for _, cellSpec := range strings.Split(rowSpec, ",") {
			w, err := strconv.Atoi(strings.TrimSpace(cellSpec))
			if err != nil || w < 1 {
				continue
			}
			row = append(row, telego.InlineKeyboardButton{
				Text:         labelAtWidth("Беговой чат жителей", w),
				CallbackData: "probe",
			})
		}
		if len(row) > 0 {
			kb = append(kb, row)
		}
	}
	return kb
}

// triLimits — лимиты ряда из 3 кнопок, которые проверяем. Для каждого
// строим 3 кнопки: на 1 меньше максимума, ровно максимум и на 1 больше
// (уже обрезанная). Так видно границу, где клиент начинает резать.
var triLimits = []int{11, 12, 13}

// triRows — ряды по 3 кнопки вокруг лимита: max-1 / max / max+1.
//
// Все три кнопки ряда ОДИНАКОВОЙ подписи (ширины max-1, max, max+1
// соответственно): ряд читается как линейка, и сразу видно, на каком
// значении клиент добавляет своё многоточие.
func triRows() [][]telego.InlineKeyboardButton {
	base := "Беговой чат жителей"
	var kb [][]telego.InlineKeyboardButton
	for _, max := range triLimits {
		row := make([]telego.InlineKeyboardButton, 0, 3)
		for _, w := range []int{max - 1, max, max + 1} {
			row = append(row, telego.InlineKeyboardButton{
				Text:         labelExact(base, w),
				CallbackData: "probe",
			})
		}
		kb = append(kb, row)
	}
	return kb
}

// seqBase — контрольная последовательность из 43 символов: русский алфавит
// от «а» до «я» (33 буквы, включая «ё») плюс «1234567890» (10 цифр).
//
// Нужна ОДНА и та же строка во всех замерах, чтобы ширина считалась одинаково
// и цифры на кнопках читались как позиции в строке. Если ширина больше 43 —
// последовательность зацикливается (см. seqLabel).
const seqBase = "абвгдеёжзийклмнопрстуфхцчшщъыьэюя1234567890"

// seqPlan — контрольные замеры по всем размерам ряда: для ряда из per кнопок
// строим подписи шириной limit-1 / limit / limit+1 вокруг текущего бюджета
// textwidth.LabelBudget(per). Средняя кнопка = ровно бюджет, боковые показывают
// границу: на limit+1 клиент уже режет, на limit-1 ещё нет.
//
// Каждый ряд подписи — копия ОДНОЙ и той же строки из seqBase, чтобы ширины
// внутри ряда совпадали и ряд читался как линейка.
var seqPlan = []int{1, 2, 3, 4, 5}

// seqLabel даёт подпись ровно w единиц ширины из seqBase, зацикливая
// последовательность, если w больше её длины (43).
func seqLabel(w int) string {
	if w <= 0 {
		return ""
	}
	if textwidth.Width(seqBase) >= w {
		return textwidth.Exact(seqBase, w)
	}
	s := ""
	for textwidth.Width(s) < w {
		s += seqBase
	}
	return textwidth.Exact(s, w)
}

// parseSizes превращает флаг -sizes ("1,3,5") в список размеров ряда.
// Мусор и размеры вне диапазона 1..6 молча отбрасываются — тест не должен
// падать из-за опечатки в флаге. Если валидных размеров не остался —
// возвращаем все пять, чтобы результат не был пустым.
func parseSizes(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > 6 {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return []int{1, 2, 3, 4, 5}
	}
	return out
}

// seqRows строит тест: по три ряда (ширина limit-1 / limit / limit+1) на каждый
// размер ряда из sizes. Кнопок в ряду — per, все подписи одной ширины.
//
// limit > 0 перебивает бюджет из таблицы — им пользуются, чтобы проверить
// ещё НЕ зафиксированный лимит (например, 32/33/34 для одной кнопки).
func seqRows(sizes []int, limit int) [][]telego.InlineKeyboardButton {
	var kb [][]telego.InlineKeyboardButton
	for _, per := range sizes {
		l := limit
		if l <= 0 {
			l = textwidth.LabelBudget(per)
		}
		for _, w := range []int{l - 1, l, l + 1} {
			row := make([]telego.InlineKeyboardButton, 0, per)
			for i := 0; i < per; i++ {
				row = append(row, telego.InlineKeyboardButton{
					Text:         seqLabel(w),
					CallbackData: "probe",
				})
			}
			kb = append(kb, row)
		}
	}
	return kb
}

// emojiSeqLabel — подпись заданной ширины w, где всё эмодзи, кроме хвоста
// из букв: 16 смайликов (32) + 1 буква = 33 и т.д. Эмодзи шириной 2, буква 1.
func emojiSeqLabel(w int) string {
	if w < 1 {
		return ""
	}
	emojis := (w - 1) / 2
	letters := w - 2*emojis
	return strings.Repeat("\U0001F525", emojis) + string([]rune(seqBase)[:letters])
}

// eseqRows — те же ряды, что и seqRows, но подпись собрана из эмодзи.
// Нужно, чтобы проверить, считает ли клиент смайлики за 2 единицы ширины.
func eseqRows(sizes []int, limit int) [][]telego.InlineKeyboardButton {
	var kb [][]telego.InlineKeyboardButton
	for _, per := range sizes {
		l := limit
		if l <= 0 {
			l = textwidth.LabelBudget(per)
		}
		for _, w := range []int{l - 1, l, l + 1} {
			row := make([]telego.InlineKeyboardButton, 0, per)
			for i := 0; i < per; i++ {
				row = append(row, telego.InlineKeyboardButton{
					Text:         emojiSeqLabel(w),
					CallbackData: "probe",
				})
			}
			kb = append(kb, row)
		}
	}
	return kb
}

// keycapBase — цифровые клавиши-эмодзи: 0️⃣…9️⃣ плюс #️⃣ и *️⃣.
//
// uniseg по основе (ASCII-цифра) даёт им ширину 1, клиент рисует клавишей
// шириной 2 — так и замерено вживую этим же режимом. Наша метрика
// (textwidth.graphemeWidth) клавишу уже считает за 2, поэтому режим
// проверяет не расхождение метрики с клиентом, а то, что клиентская
// граница по клавишам совпадает с буквенной.
var keycapBase = []string{"0️⃣", "1️⃣", "2️⃣", "3️⃣", "4️⃣", "5️⃣", "6️⃣", "7️⃣", "8️⃣", "9️⃣", "#️⃣", "*️⃣"}

// keycapLabel — подпись из клавиш-эмодзи шириной НЕ МЕНЕЕ w единиц (по нашей
// метрике). Клавиша = 2 единицы и она атомарна, разрезать её нельзя, поэтому
// реальная ширина всегда чётная и получается округлением вверх: при w=5 выйдет
// 3 клавиши = 6 единиц. Так честнее, чем заявлять точное совпадение и врать
// оператору, который смотрит на границу клиента.
func keycapLabel(w int) string {
	if w < 1 {
		return ""
	}
	n := (w + 1) / 2
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(keycapBase[i%len(keycapBase)])
	}
	return sb.String()
}

// kseqRows — по три ряда (limit-1 / limit / limit+1) на каждый размер ряда,
// подпись из клавиш-эмодзи, ширина — по НАШЕЙ метрике.
func kseqRows(sizes []int, limit int) [][]telego.InlineKeyboardButton {
	var kb [][]telego.InlineKeyboardButton
	for _, per := range sizes {
		l := limit
		if l <= 0 {
			l = textwidth.LabelBudget(per)
		}
		for _, w := range []int{l - 1, l, l + 1} {
			row := make([]telego.InlineKeyboardButton, 0, per)
			for i := 0; i < per; i++ {
				row = append(row, telego.InlineKeyboardButton{
					Text:         keycapLabel(w),
					CallbackData: "probe",
				})
			}
			kb = append(kb, row)
		}
	}
	return kb
}

// settingsRows — превью экрана «Настройки чата»: ряды ОДИН В ОДИН повторяют
// renderChatSettings из internal/bot/menu.go, но со фиксированным состоянием.
//
// Зачем: показать новый порядок рядов и правило «опции только под включённой
// фичей» БЕЗ деплоя. Подписи кладутся как есть, без нашей обрезки — превью
// должно показывать, поставит ли многоточие именно КЛИЕНТ.
//
// state = "on" — ИИ-антиспам и карантин включены, пресеты видны;
// state = "off" — оба выключены, пресеты скрыты.
//
// Активные (выбранные) значения помечены «• », как в боте. Если реальные
// дефолты чата другие — это чисто визуальная разметка превью, на код прод
// она не влияет.
func settingsRows(state string) [][]telego.InlineKeyboardButton {
	on := state != "off"
	toggle := func(prefix string) string {
		if on {
			return "✅ " + prefix
		}
		return "❌ " + prefix
	}
	row := func(texts ...string) []telego.InlineKeyboardButton {
		out := make([]telego.InlineKeyboardButton, 0, len(texts))
		for _, t := range texts {
			out = append(out, telego.InlineKeyboardButton{Text: t, CallbackData: "probe"})
		}
		return out
	}

	kb := [][]telego.InlineKeyboardButton{
		row("• 🟢Кружки", "🦋Эмодзи", "🖼Фото"),
		row("2х", "3х", "• 5х", "10х"),
		row("1мин", "• 2мин", "3мин"),
		row(toggle("🎉Приветствие"), "✏️ Свой текст"),
		row(toggle("📊 Сводка")),
		row("• 00", "04", "08", "12", "16", "20"),
		row(toggle("😴 Анонс молчунов")),
		row(toggle("🤖 ИИ-антиспам")),
	}
	if on {
		kb = append(kb,
			row("• 5 смс", "10 смс", "20 смс"),
			row("• 2 гол.", "3 гол.", "5 гол."))
	}
	kb = append(kb,
		row(toggle("💬Ждать ответ"), toggle("👻 Эфемерно")),
		row(toggle("🧫 Карантин новичков")))
	if on {
		kb = append(kb, row("• 1 час", "1 день", "1 неделя"))
	}
	kb = append(kb,
		row("🚪 Выйти из чата"),
		row("⬅️ К статистике"))
	return kb
}

func buildKeyboard(mode string, budget, rowsN, rowSize int, sizes string, limit int, widths string, edgeFrom, edgeTo, perRow int, state string) [][]telego.InlineKeyboardButton {
	switch mode {
	case "settings":
		return settingsRows(state)
	case "pair":
		return pairRows(widths)
	case "tri":
		return triRows()
	case "seq":
		return seqRows(parseSizes(sizes), limit)
	case "eseq":
		return eseqRows(parseSizes(sizes), limit)
	case "kseq":
		return kseqRows(parseSizes(sizes), limit)
	case "edge":
		return edgeRows(edgeFrom, edgeTo, perRow, budget)
	case "probe":
		return probeRows()
	case "width":
		return widthRows(budget, rowSize)
	case "emoji":
		return emojiRows(budget)
	case "rows":
		return rowsOfN(budget, rowsN)
	default:
		return mixedRows(budget)
	}
}

// labelRows собирает ряды из готовых подписей, по одной кнопке в ряд.
func labelRows(labels []label) [][]telego.InlineKeyboardButton {
	kb := make([][]telego.InlineKeyboardButton, 0, len(labels))
	for _, l := range labels {
		kb = append(kb, []telego.InlineKeyboardButton{
			{Text: l.text, CallbackData: "probe"},
		})
	}
	return kb
}

// chatLadder — реальные названия чатов, отсортированные по ВИЗУАЛЬНОЙ
// ширине (см. textwidth.Width). Служебные точки/заполнители не годятся:
// их ширина не соответствует ширине кириллицы в шрифте Telegram, и граница
// обрезки по ним смещается.
var chatLadder = []string{
	"Чат",
	"Наш двор",
	"Чат двора",
	"Беговой чат",
	"Чат жителей",
	"Чат двора №5",
	"Дворовый чат",
	"Роддом 129, июль",
	"Беговой чат двора",
	"Чат жителей Перово",
	"Беговой чат жителей",
	"Чат жителей Коньково",
	"Чат жителей Измайлово",
	"Роддом 129, июль-сентябрь",
	"Чат жителей Измайловского",
	"Беговой чат жителей Перово",
	"Беговой чат жителей Коньково",
	"Беговой чат жителей Измайлово",
	"Чат жителей Измайловского р-на",
	"Соседский чат жителей Измайлово",
	"Беговой чат жителей Измайловского",
	"Очень длинный чат жителей Измайлово",
	"Беговой чат жителей Измайловского р-на",
	"Беговой чат жителей Измайловского района",
}

// widthRows — ladder реальных названий, обрезанных до бюджета. Сверху вниз
// ширина растёт: видно, где влезает целиком, а где клиент ставит «…».
func widthRows(budget, rowSize int) [][]telego.InlineKeyboardButton {
	labels := make([]label, 0, len(chatLadder))
	for _, src := range chatLadder {
		out := textwidth.Truncate(src, budget)
		labels = append(labels, label{
			text: out,
		})
	}
	if rowSize <= 1 {
		return labelRows(labels)
	}
	return packRows(labels, rowSize)
}

// emojiRows — эмодзи в разных позициях, обрезка по бюджету.
//
// Каждая строка — отдельный случай: сами эмодзи, кластеры разной ширины,
// эмодзи в начале/конце/чередованием с буквами.
func emojiRows(budget int) [][]telego.InlineKeyboardButton {
	texts := []string{
		"🚀🔥🎉",
		strings.Repeat("🔥", 30),
		strings.Repeat("👨‍👩‍👧‍👦", 8),
		strings.Repeat("🇷🇺", 15),
		"Беговой чат жителей 🚀🔥🎉",
		"Роддом 129 👶🍼🤰",
		"🚀 Беговой чат жителей 🔥",
		"Беговой чат жителей Измайловского района 🚀🔥🎉",
		"🔥" + strings.Repeat("х", 20),
		"х🚀х🔥х🎉",
	}
	labels := make([]label, 0, len(texts))
	for _, t := range texts {
		labels = append(labels, label{text: textwidth.Truncate(t, budget)})
	}
	return labelRows(labels)
}

// rowsOfN — ladder реальных названий, разложенный рядами по n кнопок.
// Проверяет, как клиент теснит подписи, когда на ряду несколько кнопок:
// ширина ряда делится между кнопками, поэтому лимит на кнопку тут строже.
func rowsOfN(budget, n int) [][]telego.InlineKeyboardButton {
	if n < 2 {
		n = 2
	}
	if n > 6 {
		n = 6
	}
	labels := make([]label, 0, len(chatLadder))
	for _, src := range chatLadder {
		out := textwidth.Truncate(src, budget)
		labels = append(labels, label{
			text: out,
		})
	}
	return packRows(labels, n)
}

// mixedRows — короткий обзорный набор: обрезка + эмодзи + чередование.
func mixedRows(budget int) [][]telego.InlineKeyboardButton {
	base := "Беговой чат жителей Измайловского района"
	var labels []label
	for _, src := range []string{
		base,
		"129 роддом, июль-сентябрь",
		"🚀 " + base,
		"👨‍👩‍👧‍👦 " + base,
		"🇷🇺 " + base,
		base + " 🚀🔥",
		"🔥" + strings.Repeat("б", 20),
		"Беговой чат жителей Перово",
		"Беговой чат жителей Коньково",
	} {
		labels = append(labels, label{text: textwidth.Truncate(src, budget)})
	}
	return labelRows(labels)
}

// packRows раскладывает подписи рядами по n кнопок.
func packRows(labels []label, n int) [][]telego.InlineKeyboardButton {
	var kb [][]telego.InlineKeyboardButton
	var row []telego.InlineKeyboardButton
	for _, l := range labels {
		row = append(row, telego.InlineKeyboardButton{Text: l.text, CallbackData: "probe"})
		if len(row) == n {
			kb = append(kb, row)
			row = nil
		}
	}
	if len(row) > 0 {
		kb = append(kb, row)
	}
	return kb
}

func fatal(what string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", what, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", what)
	os.Exit(1)
}
