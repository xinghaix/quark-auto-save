package qas

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestWebUIStackAndI18n(t *testing.T) {
	root := filepath.Join("..", "..")
	index := readUI(t, filepath.Join(root, "app/templates/index.html"))
	login := readUI(t, filepath.Join(root, "app/templates/login.html"))
	html := index + login
	if strings.Contains(strings.ToLower(html), "jquery") {
		t.Fatal("jquery still referenced")
	}
	for _, banned := range []string{"vue@2", "new Vue(", "this.$set"} {
		if strings.Contains(html, banned) {
			t.Fatalf("banned %q", banned)
		}
	}
	for _, need := range []string{"Vue.createApp", "qasLoadJsonEditor", `class="login-shell"`} {
		if !strings.Contains(html, need) {
			t.Fatalf("missing %q", need)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "app/static/js/jquery-3.5.1.slim.min.js")); err == nil {
		t.Fatal("jquery still shipped")
	}
	js := readUI(t, filepath.Join(root, "app/static/js/i18n.js"))
	zh := i18nKeys(js, "zh")
	en := i18nKeys(js, "en")
	if len(zh) == 0 || !equalSet(zh, en) {
		t.Fatalf("i18n mismatch zh=%d en=%d", len(zh), len(en))
	}
}

func readUI(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func i18nKeys(js, name string) map[string]bool {
	marker := "\n  " + name + ": {"
	start := strings.Index(js, marker)
	if start < 0 {
		return nil
	}
	rest := js[start+len(marker):]
	end := strings.Index(rest, "\n  }")
	if end < 0 {
		return nil
	}
	keys := map[string]bool{}
	for _, item := range regexp.MustCompile(`(?m)^\s{4}([A-Za-z0-9_]+):`).FindAllStringSubmatch(rest[:end], -1) {
		keys[item[1]] = true
	}
	return keys
}

func equalSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
