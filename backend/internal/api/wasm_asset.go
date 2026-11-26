package api

import (
	"io/fs"
	"net/http"
	"os"
)

// serveWasm answers for the browser's OpenPGP engine, ~9 MB raw. The build
// writes .br and .gz beside it (frontend/vite.config.ts) because static files
// are otherwise sent as they are on disk; send the smallest one the client
// accepts, falling back to the raw file.
func serveWasm(w http.ResponseWriter, r *http.Request, root *os.Root, relPath string, info fs.FileInfo, raw *os.File) {
	h := w.Header()
	h.Set("Content-Type", "application/wasm")
	h.Add("Vary", "Accept-Encoding")
	accept := r.Header.Get("Accept-Encoding")
	for _, enc := range []struct{ coding, ext string }{{"br", ".br"}, {"gzip", ".gz"}} {
		if !acceptsEncoding(accept, enc.coding) {
			continue
		}
		f, err := root.Open(relPath + enc.ext)
		if err != nil {
			continue
		}
		defer f.Close()
		if st, err := f.Stat(); err == nil && !st.IsDir() {
			h.Set("Content-Encoding", enc.coding)
			http.ServeContent(w, r, info.Name(), info.ModTime(), f)
			return
		}
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), raw)
}
