package web

import "embed"

//go:embed index.html settings.html error.html style.css llms.txt skill.md migration.md app-icon.png
var FS embed.FS
