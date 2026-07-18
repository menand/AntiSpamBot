// Package antispam держит корневые embed-ассеты репозитория: go:embed не
// умеет подниматься выше директории пакета, поэтому CHANGELOG.md вшивается
// здесь, а internal/bot его импортирует (команда /whatsnew и оповещение о
// новой версии).
package antispam

import _ "embed"

//go:embed CHANGELOG.md
var ChangelogMD string
