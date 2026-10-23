package ebookrender_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/ebookrender"
)

func epub(t *testing.T, overrides map[string]string) []byte {
	t.Helper()
	files := map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OPS/book.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`,
		"OPS/book.opf":           `<package xmlns="http://www.idpf.org/2007/opf" version="3.0"><manifest><item id="one" href="one.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="one"/></spine></package>`,
		"OPS/one.xhtml":          `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Hidden</title></head><body><p>Hello <em>世界</em>.</p><p>Next.</p></body></html>`,
	}
	for k, v := range overrides {
		files[k] = v
	}
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestReadEPUBInSpineOrderAndPaginateUnicode(t *testing.T) {
	data := epub(t, nil)
	book, err := ebookrender.Parse(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(book.Chapters) != 1 || book.Chapters[0].Text != "Hello 世界.\nNext." {
		t.Fatalf("chapters: %#v", book.Chapters)
	}
	pages, err := book.Pages(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || pages[0].Text != "Hello 世界" || pages[1].Text != ".\nNext." {
		t.Fatalf("pages: %#v", pages)
	}
	if pages[0].Location == pages[1].Location {
		t.Fatal("locations collide")
	}
	again, _ := book.Pages(4)
	if again[0].Location != pages[0].Location {
		t.Fatal("location depends on page size")
	}
}

func TestRejectUnsafeAndUnsupportedEPUB(t *testing.T) {
	cases := map[string]map[string]string{
		"archive traversal":           {"../escape": "no"},
		"absolute archive path":       {"/escape": "no"},
		"encrypted":                   {"META-INF/encryption.xml": "<encryption/>"},
		"manifest external":           {"OPS/book.opf": `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="one" href="https://example.invalid/a"/></manifest><spine><itemref idref="one"/></spine></package>`},
		"duplicate manifest":          {"OPS/book.opf": `<package xmlns="http://www.idpf.org/2007/opf"><manifest/><manifest><item id="one" href="one.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="one"/></spine></package>`},
		"wrong manifest namespace":    {"OPS/book.opf": `<package xmlns="http://www.idpf.org/2007/opf"><manifest xmlns="urn:wrong"><item id="one" href="one.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="one"/></spine></package>`},
		"spine missing":               {"OPS/book.opf": `<package xmlns="http://www.idpf.org/2007/opf"><manifest/><spine><itemref idref="missing"/></spine></package>`},
		"aliased spine amplification": {"OPS/book.opf": `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="one" href="one.xhtml" media-type="application/xhtml+xml"/><item id="alias" href="one.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="one"/><itemref idref="alias"/></spine></package>`},
		"repeated spine":              {"OPS/book.opf": `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="one" href="one.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="one"/><itemref idref="one"/></spine></package>`},
		"malformed XHTML":             {"OPS/one.xhtml": `<html xmlns="http://www.w3.org/1999/xhtml"><body><p>bad</body></html>`},
		"multiple roots":              {"OPS/one.xhtml": `<html/><html/>`},
		"DTD":                         {"OPS/one.xhtml": `<!DOCTYPE html [<!ENTITY e SYSTEM "file:///etc/passwd">]><html><body>&e;</body></html>`},
		"external image":              {"OPS/one.xhtml": `<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="https://example.invalid/a"/></body></html>`},
		"fixed layout":                {"OPS/book.opf": `<package xmlns="http://www.idpf.org/2007/opf"><metadata><meta property="rendition:layout">pre-paginated</meta></metadata><manifest/><spine/></package>`},
		"deep XML":                    {"OPS/one.xhtml": strings.Repeat("<a>", 129) + strings.Repeat("</a>", 129)},
		"large entry":                 {"unused": strings.Repeat("a", ebookrender.MaxEntryBytes+1)},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			data := epub(t, override)
			if _, err := ebookrender.Parse(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil {
				t.Fatal("unsafe EPUB accepted")
			}
		})
	}
}

func TestCanceledAndOversizedInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := epub(t, nil)
	if _, err := ebookrender.Parse(ctx, bytes.NewReader(data), int64(len(data))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error: %v", err)
	}
	if _, err := ebookrender.Parse(context.Background(), bytes.NewReader(nil), ebookrender.MaxArchiveBytes+1); !errors.Is(err, ebookrender.ErrLimit) {
		t.Fatalf("size error: %v", err)
	}
}

func TestRejectDuplicateZIPEntriesAndExpandedBudget(t *testing.T) {
	for _, duplicate := range []bool{true, false} {
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		for i := 0; i < 9; i++ {
			name := fmt.Sprintf("part%d", i)
			if duplicate {
				name = "same"
			}
			f, err := w.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write([]byte(strings.Repeat("x", ebookrender.MaxEntryBytes))); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := ebookrender.Parse(context.Background(), bytes.NewReader(b.Bytes()), int64(b.Len())); err == nil {
			t.Fatalf("accepted duplicate=%v", duplicate)
		}
	}
}

func TestSpineOrderAndIgnoredExecutableContent(t *testing.T) {
	data := epub(t, map[string]string{
		"OPS/book.opf":  `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="one" href="one.xhtml" media-type="application/xhtml+xml"/><item id="two" href="two.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="two"/><itemref idref="one"/></spine></package>`,
		"OPS/two.xhtml": `<html xmlns="http://www.w3.org/1999/xhtml"><body><script>bad()</script><style>hidden</style><p>First.</p></body></html>`,
	})
	book, err := ebookrender.Parse(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(book.Chapters) != 2 || book.Chapters[0].Text != "First." || book.Chapters[1].Path != "OPS/one.xhtml" {
		t.Fatalf("chapters: %#v", book.Chapters)
	}
}

func TestPageLimits(t *testing.T) {
	book := ebookrender.Book{Chapters: []ebookrender.Chapter{{Text: strings.Repeat("x", ebookrender.MaxPages+1)}}}
	for _, size := range []int{0, 16385, 1} {
		if _, err := book.Pages(size); !errors.Is(err, ebookrender.ErrLimit) {
			t.Fatalf("size %d: %v", size, err)
		}
	}
}
