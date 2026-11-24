package sources

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
	"golang.org/x/text/encoding/htmlindex"
)

// Feed fetches an RSS 2.0 or Atom feed.
type Feed struct {
	Client *http.Client
	Name   string
	URL    string
}

type rssDoc struct {
	Items []struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		GUID        string `xml:"guid"`
		Description string `xml:"description"`
		Comments    string `xml:"comments"`
		Author      string `xml:"author"`
		Creator     string `xml:"http://purl.org/dc/elements/1.1/ creator"`
		PubDate     string `xml:"pubDate"`
	} `xml:"channel>item"`
}

type atomDoc struct {
	Entries []struct {
		Title string `xml:"title"`
		ID    string `xml:"id"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
		Summary   string `xml:"summary"`
		Content   string `xml:"content"`
		Author    string `xml:"author>name"`
		Published string `xml:"published"`
		Updated   string `xml:"updated"`
	} `xml:"entry"`
}

// Fetch downloads and parses the feed.
func (f *Feed) Fetch(ctx context.Context) ([]store.Item, error) {
	body, err := get(ctx, f.Client, f.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("feed %s: %w", f.Name, err)
	}
	items, err := ParseFeed(f.Name, body)
	if err != nil {
		return nil, fmt.Errorf("feed %s: %w", f.Name, err)
	}
	return items, nil
}

// ParseFeed parses RSS 2.0 or Atom XML into items tagged with source.
func ParseFeed(source string, body []byte) ([]store.Item, error) {
	var root struct{ XMLName xml.Name }
	if err := decodeXML(body, &root); err != nil {
		return nil, fmt.Errorf("parse xml: %w", err)
	}
	var out []store.Item
	switch root.XMLName.Local {
	case "rss":
		var d rssDoc
		if err := decodeXML(body, &d); err != nil {
			return nil, fmt.Errorf("parse rss: %w", err)
		}
		for _, it := range d.Items {
			author := it.Author
			if author == "" {
				author = it.Creator
			}
			out = appendItem(out, source, first(it.GUID, it.Link), it.Title, it.Link, it.Comments,
				author, it.Description, parseTime(it.PubDate))
		}
	case "feed":
		var d atomDoc
		if err := decodeXML(body, &d); err != nil {
			return nil, fmt.Errorf("parse atom: %w", err)
		}
		for _, e := range d.Entries {
			link := ""
			for _, l := range e.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					link = l.Href
					break
				}
			}
			out = appendItem(out, source, first(e.ID, link), e.Title, link, "", e.Author,
				first(e.Summary, e.Content), parseTime(first(e.Published, e.Updated)))
		}
	default:
		return nil, fmt.Errorf("unknown feed type %q", root.XMLName.Local)
	}
	return out, nil
}

// decodeXML decodes leniently: real-world feeds use HTML entities such as
// &rsquo; and non-UTF-8 encodings such as ISO-8859-1.
func decodeXML(body []byte, v any) error {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	d.CharsetReader = func(label string, r io.Reader) (io.Reader, error) {
		e, err := htmlindex.Get(label)
		if err != nil {
			return nil, fmt.Errorf("charset %q: %w", label, err)
		}
		return e.NewDecoder().Reader(r), nil
	}
	return d.Decode(v)
}

func appendItem(out []store.Item, source, id, title, link, comments, author, text string, pub time.Time) []store.Item {
	link = strings.TrimSpace(link)
	title = clip(plain(title), 300)
	if !isWebURL(link) || title == "" {
		return out
	}
	if !isWebURL(strings.TrimSpace(comments)) || strings.TrimSpace(comments) == link {
		comments = ""
	}
	return append(out, store.Item{
		Source:     source,
		ExtID:      clip(strings.TrimSpace(id), 500),
		Title:      title,
		URL:        link,
		DiscussURL: strings.TrimSpace(comments),
		Author:     clip(plain(author), 100),
		Text:       clip(plain(text), maxTextRunes),
		Published:  pub,
	})
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

var timeLayouts = []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST"}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
