package web

import "embed"

//go:embed index.html index.md robots.txt sitemap.xml 3aff9d89fda08c367b45d279593f7ce1.txt settings.html error.html nav.html dashboard.html project.html style.css app.js ga.js llms.txt skill.md migration.md changelog.md about.html about.md app-icon.png
var FS embed.FS
