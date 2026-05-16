package website

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

//go:embed all:out
var embedded embed.FS

type Website interface {
	HTTPHandler() http.Handler
}

type staticWebsite struct {
	handler http.Handler
}

func NewWebsite() (Website, error) {
	out, err := fs.Sub(embedded, "out")
	if err != nil {
		return nil, err
	}

	return &staticWebsite{
		handler: spaHandler{fsys: http.FS(out)},
	}, nil
}

func NewDirectoryWebsite(root string) Website {
	return &staticWebsite{
		handler: spaHandler{fsys: http.Dir(root)},
	}
}

func (w *staticWebsite) HTTPHandler() http.Handler {
	return w.handler
}

type spaHandler struct {
	fsys http.FileSystem
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	requestPath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if requestPath == "." || requestPath == "" {
		requestPath = "index.html"
	}

	if h.servePath(w, r, requestPath) {
		return
	}

	if isAssetPath(requestPath) {
		http.NotFound(w, r)
		return
	}

	if h.servePath(w, r, "index.html") {
		return
	}

	http.Error(w, "frontend build not found", http.StatusNotFound)
}

func Exists(root string) bool {
	if root == "" {
		return false
	}
	info, err := os.Stat(root)
	return err == nil && info.IsDir()
}

func (h spaHandler) servePath(w http.ResponseWriter, r *http.Request, name string) bool {
	file, err := h.fsys.Open(name)
	if err != nil {
		return false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return false
	}

	if info.IsDir() {
		indexPath := path.Join(name, "index.html")
		if indexPath == name {
			return false
		}
		return h.servePath(w, r, indexPath)
	}

	if strings.HasPrefix(name, "_next/static/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else if strings.HasSuffix(name, ".html") {
		w.Header().Set("Cache-Control", "no-store")
	}

	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	return true
}

func isAssetPath(name string) bool {
	return strings.HasPrefix(name, "_next/") ||
		strings.HasPrefix(name, "assets/") ||
		path.Ext(name) != ""
}
