package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"reflect"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// Assets serves static files under a base path, with a content hash in every URL so
// browsers can cache them for a year. A request for an outdated hash gets the current
// file uncached, so pages rendered by the previous release keep working during a
// rolling deploy.
type Assets struct {
	base     string
	mu       sync.RWMutex
	files    map[string]asset
	prefixes map[string]fs.FS
}

type asset struct {
	data        []byte
	hash        string
	contentType string
}

// NewAssets returns assets served under base, such as "/assets". It already holds the
// default design's files under the "ui" prefix.
func NewAssets(base string) *Assets {
	a := &Assets{base: strings.TrimSuffix(base, "/"), files: map[string]asset{}, prefixes: map[string]fs.FS{}}
	if err := a.Add("ui", files()); err != nil {
		panic(err)
	}
	return a
}

// Add serves every file in files under prefix: files/app.css becomes "prefix/app.css".
// Adding the same files again is a no-op; adding other files under a used prefix is an
// error.
func (a *Assets) Add(prefix string, files fs.FS) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, ok := a.prefixes[prefix]; ok {
		if reflect.TypeOf(files).Comparable() && existing == files {
			return nil
		}
		return fmt.Errorf("ui: assets with prefix %q were already added", prefix)
	}
	a.prefixes[prefix] = files
	return fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return err
		}
		data, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		contentType := mime.TypeByExtension(path.Ext(p))
		if contentType == "" {
			contentType = http.DetectContentType(data)
		}
		a.files[prefix+"/"+p] = asset{data: data, hash: hex.EncodeToString(sum[:6]), contentType: contentType}
		return nil
	})
}

// URL returns the hashed URL of a file, such as "/assets/3f2a9c01be44/ui/base.css".
// It panics for an unknown name, which is a programming error.
func (a *Assets) URL(name string) string {
	a.mu.RLock()
	f, ok := a.files[name]
	a.mu.RUnlock()
	if !ok {
		panic(fmt.Sprintf("ui: unknown asset %q", name))
	}
	return a.base + "/" + f.hash + "/" + name
}

// Serve handles GET requests under the base path. Register it with a wildcard:
//
//	r.GET("/assets/*path", assets.Serve)
func (a *Assets) Serve(c *gin.Context) {
	hash, name, ok := strings.Cut(strings.TrimPrefix(c.Param("path"), "/"), "/")
	a.mu.RLock()
	f, found := a.files[name]
	a.mu.RUnlock()
	if !ok || !found {
		c.Status(http.StatusNotFound)
		return
	}
	if hash == f.hash {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		c.Header("Cache-Control", "no-cache")
	}
	c.Data(http.StatusOK, f.contentType, f.data)
}
