package website

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedWebsiteServesIndexWithoutRedirect(t *testing.T) {
	site, err := NewWebsite()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	site.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if location := rec.Header().Get("Location"); location != "" {
		t.Fatalf("unexpected redirect to %q", location)
	}
	if cacheControl := rec.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("expected index cache-control no-store, got %q", cacheControl)
	}
}

func TestEmbeddedWebsiteDoesNotFallbackForMissingNextAssets(t *testing.T) {
	site, err := NewWebsite()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/_next/static/chunks/missing.js", nil)
	rec := httptest.NewRecorder()

	site.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rec.Code)
	}
	if location := rec.Header().Get("Location"); location != "" {
		t.Fatalf("unexpected redirect to %q", location)
	}
}

func TestEmbeddedWebsiteServesNextAssets(t *testing.T) {
	site, err := NewWebsite()
	if err != nil {
		t.Fatal(err)
	}

	assetPath := findEmbeddedNextAsset(t)
	req := httptest.NewRequest(http.MethodGet, "/"+strings.TrimPrefix(assetPath, "out/"), nil)
	rec := httptest.NewRecorder()

	site.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200 for %s, got %d", assetPath, rec.Code)
	}
	if location := rec.Header().Get("Location"); location != "" {
		t.Fatalf("unexpected redirect to %q", location)
	}
	if cacheControl := rec.Header().Get("Cache-Control"); cacheControl != "public, max-age=31536000, immutable" {
		t.Fatalf("expected immutable asset cache-control, got %q", cacheControl)
	}
}

func TestEmbeddedWebsiteFallsBackToIndexForAppRoutes(t *testing.T) {
	site, err := NewWebsite()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions/local", nil)
	rec := httptest.NewRecorder()

	site.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if location := rec.Header().Get("Location"); location != "" {
		t.Fatalf("unexpected redirect to %q", location)
	}
}

func findEmbeddedNextAsset(t *testing.T) string {
	t.Helper()

	var assetPath string
	err := fs.WalkDir(embedded, "out/_next/static", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(name, ".js") {
			assetPath = name
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if assetPath == "" {
		t.Fatal("expected at least one embedded _next static asset")
	}

	return assetPath
}
