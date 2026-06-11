package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web/index.html web/app.js
var webFS embed.FS

// webHandler serves the embedded single-page app. index.html is served at "/"
// and app.js at "/app.js"; everything else under "/" falls through to index so
// the SPA can own client-side routing.
func webHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // embedded FS is known-good at build time
	}
	files := http.FileServer(http.FS(sub))
	mux := http.NewServeMux()
	mux.Handle("/app.js", files)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFileFS(w, r, sub, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	})
	return mux
}
