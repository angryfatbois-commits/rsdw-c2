package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedIndexVersionsAssets(t *testing.T) {
	app := newTestApp(t, false)
	for _, target := range []string{"/", "/index.html", "/?refresh=1", "/./index.html"} {
		res := httptest.NewRecorder()
		app.ServeHTTP(res, httptest.NewRequest(http.MethodGet, target, nil))
		if res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: status=%d cache-control=%q", target, res.Code, res.Header().Get("Cache-Control"))
		}
		for _, name := range []string{"app.js", "styles.css"} {
			if strings.Contains(res.Body.String(), `"/`+name+`"`) {
				t.Fatalf("%s: unversioned %s reference", target, name)
			}
			pattern := regexp.MustCompile(`"(/` + regexp.QuoteMeta(name) + `\?v=[a-f0-9]{64})"`)
			match := pattern.FindStringSubmatch(res.Body.String())
			if match == nil {
				t.Fatalf("%s: missing content hash for %s", target, name)
			}
			asset := httptest.NewRecorder()
			app.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, match[1], nil))
			want, err := webFiles.ReadFile("web/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if asset.Code != http.StatusOK || asset.Body.String() != string(want) {
				t.Fatalf("%s: response does not match embedded asset, status=%d", match[1], asset.Code)
			}
			if contentType := asset.Header().Get("Content-Type"); !strings.Contains(contentType, "javascript") && !strings.Contains(contentType, "text/css") {
				t.Fatalf("%s: content-type=%q", match[1], contentType)
			}
		}
	}
	res := httptest.NewRecorder()
	app.ServeHTTP(res, httptest.NewRequest(http.MethodHead, "/", nil))
	if res.Code != http.StatusOK || res.Body.Len() != 0 || res.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(res.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("HEAD index: status=%d headers=%v body=%q", res.Code, res.Header(), res.Body.String())
	}
}

func TestAssetURLsChangeWithContent(t *testing.T) {
	index, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	root := fstest.MapFS{
		"index.html": {Data: index},
		"app.js":     {Data: []byte("abc")},
		"styles.css": {Data: []byte("abc")},
	}
	indexBody := func() string {
		t.Helper()
		res := httptest.NewRecorder()
		newWebHandler(root).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))
		if res.Code != http.StatusOK {
			t.Fatalf("index status=%d", res.Code)
		}
		return res.Body.String()
	}
	original := indexBody()
	const abcHash = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	for _, name := range []string{"app.js", "styles.css"} {
		url := "/" + name + "?v=" + abcHash
		if !strings.Contains(original, `"`+url+`"`) {
			t.Fatalf("missing expected content-derived URL %s", url)
		}
		root[name].Data = []byte("changed")
		changed := indexBody()
		if strings.Contains(changed, `"`+url+`"`) || strings.Contains(changed, `"/`+name+`"`) {
			t.Fatalf("%s retained a stale asset reference", name)
		}
		match := regexp.MustCompile(`"(/` + regexp.QuoteMeta(name) + `\?v=[a-f0-9]{64})"`).FindStringSubmatch(changed)
		if match == nil {
			t.Fatalf("changed %s has no versioned URL", name)
		}
		asset := httptest.NewRecorder()
		newWebHandler(root).ServeHTTP(asset, httptest.NewRequest(http.MethodGet, match[1], nil))
		if asset.Code != http.StatusOK || asset.Body.String() != "changed" {
			t.Fatalf("changed asset status=%d body=%q", asset.Code, asset.Body.String())
		}
		for _, other := range []string{"app.js", "styles.css"} {
			if other != name && !strings.Contains(changed, `"/`+other+`?v=`+abcHash+`"`) {
				t.Fatalf("unchanged %s URL changed", other)
			}
		}
		root[name].Data = []byte("abc")
		if restored := indexBody(); restored != original {
			t.Fatal("identical content did not produce identical asset URLs")
		}
	}
}
