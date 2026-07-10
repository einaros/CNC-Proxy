package uiquality_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestHTMLDoesNotRepeatVisibleRegionLabels(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"internal/api/web/index.html",
		"cmd/fakemachine/web/index.html",
	} {
		t.Run(rel, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Fatal(err)
			}
			doc, err := html.Parse(strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			assertNoDetailsLabelRepeats(t, rel, doc)
			assertNoGroupLabelRepeats(t, rel, doc)
		})
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func assertNoDetailsLabelRepeats(t *testing.T, rel string, root *html.Node) {
	t.Helper()
	forEachNode(root, func(n *html.Node) {
		if n.Type != html.ElementNode || n.Data != "details" {
			return
		}
		summary := directChild(n, "summary")
		if summary == nil {
			return
		}
		summaryLabel := normalizedText(summary)
		if summaryLabel == "" {
			return
		}
		forEachDescendant(summary, func(child *html.Node) bool {
			if isHeading(child) {
				t.Errorf("%s uses heading markup inside summary %q; the summary is already the region label", rel, summaryLabel)
				return false
			}
			return true
		})
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if child == summary {
				continue
			}
			forEachStructuralTitle(child, func(title string) {
				if labelsEquivalent(summaryLabel, title) {
					t.Errorf("%s repeats details summary %q as an internal title", rel, title)
				}
			})
		}
	})
}

func assertNoGroupLabelRepeats(t *testing.T, rel string, root *html.Node) {
	t.Helper()
	forEachNode(root, func(n *html.Node) {
		if !isStructuralTitle(n) {
			return
		}
		title := normalizedText(n)
		if title == "" || n.Parent == nil {
			return
		}
		for sibling := n.NextSibling; sibling != nil; sibling = sibling.NextSibling {
			forEachFieldLabel(sibling, func(label string) {
				if labelsEquivalent(title, label) {
					t.Errorf("%s repeats group title %q as field label %q", rel, title, label)
				}
			})
		}
	})
}

func forEachNode(n *html.Node, fn func(*html.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		forEachNode(child, fn)
	}
}

func forEachDescendant(n *html.Node, fn func(*html.Node) bool) {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if !fn(child) {
			return
		}
		forEachDescendant(child, fn)
	}
}

func forEachStructuralTitle(n *html.Node, fn func(string)) {
	if n == nil {
		return
	}
	if n.Type == html.ElementNode && n.Data == "details" {
		return
	}
	if isStructuralTitle(n) {
		if title := normalizedText(n); title != "" {
			fn(title)
		}
		return
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		forEachStructuralTitle(child, fn)
	}
}

func forEachFieldLabel(n *html.Node, fn func(string)) {
	if n == nil {
		return
	}
	if n.Type == html.ElementNode {
		if n.Data == "details" || isStructuralTitle(n) {
			return
		}
		if n.Data == "label" {
			if label := labelText(n); label != "" {
				fn(label)
			}
			return
		}
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		forEachFieldLabel(child, fn)
	}
}

func directChild(n *html.Node, name string) *html.Node {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.Data == name {
			return child
		}
	}
	return nil
}

func labelText(n *html.Node) string {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.Data == "span" {
			return normalizedText(child)
		}
	}
	return normalizedText(n)
}

func normalizedText(n *html.Node) string {
	var parts []string
	var collect func(*html.Node)
	collect = func(cur *html.Node) {
		if cur.Type == html.ElementNode {
			switch cur.Data {
			case "script", "style", "template":
				return
			}
		}
		if cur.Type == html.TextNode {
			if text := strings.TrimSpace(cur.Data); text != "" {
				parts = append(parts, text)
			}
		}
		for child := cur.FirstChild; child != nil; child = child.NextSibling {
			collect(child)
		}
	}
	collect(n)
	return normalizeLabel(strings.Join(parts, " "))
}

func normalizeLabel(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

func labelsEquivalent(a, b string) bool {
	a = normalizeLabel(a)
	b = normalizeLabel(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || singularLabel(a) == singularLabel(b)
}

func singularLabel(s string) string {
	words := strings.Fields(s)
	for i, word := range words {
		if len(word) > 3 && strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") {
			words[i] = strings.TrimSuffix(word, "s")
		}
	}
	return strings.Join(words, " ")
}

func isStructuralTitle(n *html.Node) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	if isHeading(n) {
		return true
	}
	for _, class := range strings.Fields(attr(n, "class")) {
		switch class {
		case "tap-control-title", "origin-group-title", "tool-title", "section-title", "side-title":
			return true
		}
		if strings.HasSuffix(class, "-title") {
			return true
		}
	}
	return false
}

func isHeading(n *html.Node) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	return n.Data == "h1" || n.Data == "h2" || n.Data == "h3" || n.Data == "h4" || n.Data == "h5" || n.Data == "h6"
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
