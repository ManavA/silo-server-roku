// Package ebookrender extracts bounded, plain text from DRM-free reflowable EPUB.
// It performs no network requests, archive extraction, CSS layout, or script execution.
package ebookrender

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
)

const (
	MaxArchiveBytes  = 32 << 20
	MaxEntries       = 1024
	MaxEntryBytes    = 4 << 20
	MaxExpandedBytes = 32 << 20
	MaxPages         = 10000
)

var (
	ErrInvalid     = errors.New("invalid EPUB")
	ErrLimit       = errors.New("EPUB limit exceeded")
	ErrUnsupported = errors.New("unsupported EPUB")
)

type Chapter struct {
	Path string
	Text string
}
type Book struct {
	Revision string
	Chapters []Chapter
}
type Page struct {
	Text     string
	Location string
}

// Pages splits at Unicode scalar boundaries. Locations identify normalized text
// offsets, not EPUB CFI, and must not overwrite existing browser reader progress.
// The revision includes archive bytes and extraction version. No visual layout is implied.
func (b *Book) Pages(runesPerPage int) ([]Page, error) {
	if runesPerPage < 1 || runesPerPage > 16384 {
		return nil, fmt.Errorf("%w: page size", ErrLimit)
	}
	var pages []Page
	for i, ch := range b.Chapters {
		text := []rune(ch.Text)
		for offset := 0; offset < len(text); offset += runesPerPage {
			if len(pages) >= MaxPages {
				return nil, ErrLimit
			}
			end := min(offset+runesPerPage, len(text))
			pages = append(pages, Page{Text: string(text[offset:end]), Location: fmt.Sprintf("silo-text-v1:%s:%d:%d", b.Revision, i, offset)})
		}
	}
	return pages, nil
}

// Parse reads an entire bounded archive in memory; callers should additionally
// bound concurrent jobs. The context is checked during reads and XML tokenization.
func Parse(ctx context.Context, r io.ReaderAt, size int64) (*Book, error) {
	if size <= 0 {
		return nil, ErrInvalid
	}
	if size > MaxArchiveBytes {
		return nil, ErrLimit
	}
	raw, err := io.ReadAll(io.NewSectionReader(r, 0, size))
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), size)
	if err != nil {
		return nil, fmt.Errorf("%w: zip", ErrInvalid)
	}
	if len(zr.File) > MaxEntries {
		return nil, ErrLimit
	}
	files := make(map[string][]byte, len(zr.File))
	var total uint64
	for _, f := range zr.File {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(f.Name, "/")
		if !safePath(name) {
			return nil, fmt.Errorf("%w: archive path", ErrInvalid)
		}
		if _, ok := files[f.Name]; ok {
			return nil, fmt.Errorf("%w: duplicate path", ErrInvalid)
		}
		if !f.Mode().IsRegular() && !f.FileInfo().IsDir() {
			return nil, ErrUnsupported
		}
		if f.UncompressedSize64 > MaxEntryBytes {
			return nil, ErrLimit
		}
		total += f.UncompressedSize64
		if total > MaxExpandedBytes {
			return nil, ErrLimit
		}
		rc, e := f.Open()
		if e != nil {
			return nil, fmt.Errorf("%w: entry", ErrInvalid)
		}
		data, e := io.ReadAll(io.LimitReader(rc, MaxEntryBytes+1))
		closeErr := rc.Close()
		if e != nil || closeErr != nil {
			return nil, fmt.Errorf("%w: entry", ErrInvalid)
		}
		if len(data) > MaxEntryBytes {
			return nil, ErrLimit
		}
		files[f.Name] = data
	}
	if string(files["mimetype"]) != "application/epub+zip" {
		return nil, ErrUnsupported
	}
	if _, ok := files["META-INF/encryption.xml"]; ok {
		return nil, ErrUnsupported
	}
	var container struct {
		XMLName xml.Name `xml:"urn:oasis:names:tc:opendocument:xmlns:container container"`
		Roots   []struct {
			Path string `xml:"full-path,attr"`
			Type string `xml:"media-type,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err = decode(ctx, files["META-INF/container.xml"], &container); err != nil {
		return nil, err
	}
	if len(container.Roots) != 1 || container.Roots[0].Type != "application/oebps-package+xml" || !safePath(container.Roots[0].Path) {
		return nil, ErrInvalid
	}
	opf := container.Roots[0].Path
	var pkg struct {
		XMLName xml.Name `xml:"http://www.idpf.org/2007/opf package"`
		Items   []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			Type       string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"manifest>item"`
		Spine []struct {
			ID         string `xml:"idref,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"spine>itemref"`
		Meta []struct {
			Property string `xml:"property,attr"`
			Value    string `xml:",chardata"`
		} `xml:"metadata>meta"`
	}
	if err = validatePackage(ctx, files[opf]); err != nil {
		return nil, err
	}
	if err = decode(ctx, files[opf], &pkg); err != nil {
		return nil, err
	}
	for _, m := range pkg.Meta {
		if m.Property == "rendition:layout" && strings.TrimSpace(m.Value) == "pre-paginated" {
			return nil, ErrUnsupported
		}
	}
	items := map[string]string{}
	for _, item := range pkg.Items {
		if item.ID == "" {
			return nil, ErrInvalid
		}
		if _, ok := items[item.ID]; ok {
			return nil, ErrInvalid
		}
		if !safePath(item.Href) {
			return nil, ErrUnsupported
		}
		target := path.Join(path.Dir(opf), item.Href)
		if _, ok := files[target]; !ok {
			return nil, ErrInvalid
		}
		if strings.Contains(item.Properties, "remote-resources") {
			return nil, ErrUnsupported
		}
		items[item.ID] = target
	}
	if len(pkg.Spine) == 0 {
		return nil, ErrInvalid
	}
	sum := sha256.Sum256(raw)
	book := &Book{Revision: hex.EncodeToString(sum[:])}
	seenSpine := map[string]bool{}
	for _, ref := range pkg.Spine {
		if seenSpine[ref.ID] {
			return nil, ErrInvalid
		}
		seenSpine[ref.ID] = true
		target, ok := items[ref.ID]
		if !ok {
			return nil, ErrInvalid
		}
		for _, item := range pkg.Items {
			if item.ID == ref.ID && item.Type != "application/xhtml+xml" {
				return nil, ErrUnsupported
			}
		}
		if strings.Contains(ref.Properties, "rendition:layout-pre-paginated") {
			return nil, ErrUnsupported
		}
		text, e := chapterText(ctx, files[target])
		if e != nil {
			return nil, e
		}
		book.Chapters = append(book.Chapters, Chapter{Path: target, Text: text})
	}
	return book, nil
}

func safePath(s string) bool {
	u, err := url.Parse(s)
	return err == nil && s != "" && !strings.ContainsAny(s, "\\%\x00?#:") && !strings.HasPrefix(s, "/") && path.Clean(s) == s && s != "." && s != ".." && !strings.HasPrefix(s, "../") && u.Scheme == "" && u.Host == ""
}

// tokens enforces a depth bound and rejects DTD/entity declarations and processing
// instructions other than the XML declaration. encoding/xml never resolves entities.
func tokens(ctx context.Context, data []byte, visit func(xml.Token) error) error {
	d := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	roots := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrInvalid
		}
		switch v := t.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			depth++
			if depth > 128 {
				return ErrLimit
			}
		case xml.EndElement:
			depth--
		case xml.Directive:
			return ErrUnsupported
		case xml.ProcInst:
			if v.Target != "xml" {
				return ErrUnsupported
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(v)) != "" {
				return ErrInvalid
			}
		}
		if visit != nil {
			if err = visit(t); err != nil {
				return err
			}
		}
	}
	if roots != 1 || depth != 0 {
		return ErrInvalid
	}
	return nil
}
func decode(ctx context.Context, data []byte, out any) error {
	if err := tokens(ctx, data, nil); err != nil {
		return err
	}
	if err := xml.Unmarshal(data, out); err != nil {
		return ErrInvalid
	}
	return nil
}

func chapterText(ctx context.Context, data []byte) (string, error) {
	var out strings.Builder
	body := false
	seenBody := false
	depth := 0
	bodyDepth := 0
	skip := 0
	root := false
	block := func() {
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
	}
	err := tokens(ctx, data, func(token xml.Token) error {
		switch v := token.(type) {
		case xml.StartElement:
			depth++
			if !root {
				root = true
				if v.Name.Local != "html" || v.Name.Space != "http://www.w3.org/1999/xhtml" {
					return ErrUnsupported
				}
			}
			for _, a := range v.Attr {
				if a.Name.Local == "href" || a.Name.Local == "src" || a.Name.Local == "poster" || a.Name.Local == "data" {
					if a.Value != "" && !strings.HasPrefix(a.Value, "#") && !safePath(a.Value) {
						return ErrUnsupported
					}
				}
			}
			if v.Name.Local == "body" {
				if seenBody {
					return ErrInvalid
				}
				seenBody = true
				body = true
				bodyDepth = depth
			}
			if v.Name.Local == "script" || v.Name.Local == "style" {
				skip++
			}
			if body && skip == 0 {
				switch v.Name.Local {
				case "p", "div", "br", "li", "h1", "h2", "h3", "section":
					block()
				}
			}
		case xml.EndElement:
			if body && skip == 0 {
				switch v.Name.Local {
				case "p", "div", "li", "h1", "h2", "h3", "section":
					block()
				}
			}
			if v.Name.Local == "script" || v.Name.Local == "style" {
				skip--
			}
			if depth == bodyDepth {
				body = false
			}
			depth--
		case xml.CharData:
			if body && skip == 0 {
				out.Write(v)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !seenBody {
		return "", ErrInvalid
	}
	lines := strings.Split(out.String(), "\n")
	var clean []string
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			clean = append(clean, line)
		}
	}
	return strings.Join(clean, "\n"), nil
}

// validatePackage prevents local-name XML decoding from accepting namespace
// spoofing or merging duplicate package sections.
func validatePackage(ctx context.Context, data []byte) error {
	var stack []string
	manifest, spine := 0, 0
	err := tokens(ctx, data, func(t xml.Token) error {
		switch v := t.(type) {
		case xml.StartElement:
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			switch v.Name.Local {
			case "package", "manifest", "spine", "item", "itemref":
				if v.Name.Space != "http://www.idpf.org/2007/opf" {
					return ErrInvalid
				}
			}
			switch v.Name.Local {
			case "manifest":
				if parent != "package" {
					return ErrInvalid
				}
				manifest++
			case "spine":
				if parent != "package" {
					return ErrInvalid
				}
				spine++
			case "item":
				if parent != "manifest" {
					return ErrInvalid
				}
			case "itemref":
				if parent != "spine" {
					return ErrInvalid
				}
			}
			stack = append(stack, v.Name.Local)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
		return nil
	})
	if err != nil {
		return err
	}
	if manifest != 1 || spine != 1 {
		return ErrInvalid
	}
	return nil
}
