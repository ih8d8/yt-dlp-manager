// Package webui embeds the compiled frontend so the single Go binary serves
// the complete browser UI. static/ holds the committed, reproducible Vite
// output; a clean checkout builds without Node.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:static
var content embed.FS

// Handler serves the SPA. Unknown non-API routes fall back to index.html for
// client-side navigation; missing asset-like paths 404.
func Handler() http.Handler {
	sub, err := fs.Sub(content, "static")
	if err != nil {
		panic("webui: embedded static missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if p == "" || p == "." {
			p = "index.html"
		}
		if _, statErr := fs.Stat(sub, p); statErr != nil {
			if looksLikeAsset(p) {
				http.NotFound(w, r)
				return
			}
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}

func looksLikeAsset(p string) bool {
	base := path.Base(p)
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 {
		return false
	}
	ext := base[dot:]
	switch ext {
	case ".html", "":
		return false
	}
	return true
}
