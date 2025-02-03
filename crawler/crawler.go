package crawler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/TheSnook/polyester/proto/resource"
	"github.com/TheSnook/polyester/site"
	"github.com/TheSnook/polyester/storage"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const MAX_REDIRECTS = 10

// If strings appear in script bodies, they get any `https:\/\/{HOSTNAME}` prefix stripped by plain-text substitution.
var STATIC_REPLACEMENTS = []string{
	// concatemoji
	`\/wp-includes\/js\/wp-emoji-release.min.js`,
	// jetpackSwiperLibraryPath
	`\/wp-content\/plugins\/jetpack\/_inc\/build\/carousel\/swiper-bundle.min.js`,
	// Don't think we need these
	// "ajaxurl":"https:\/\/www.web-goddess.org\/wp-admin\/admin-ajax.php"
	// "login_url":"https:\/\/www.web-goddess.org\/wp-login.php?redirect_to=https%3A%2F%2Fwww.web-goddess.org%2Farchive%2F23126"
}

type Crawler struct {
	db         storage.Storage
	httpClient *http.Client
	origin     string
	visited    map[string]struct{}
}

func noRedirects(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

func New(origin string, db storage.Storage) Crawler {
	return Crawler{
		db: db,
		httpClient: &http.Client{
			CheckRedirect: noRedirects,
		},
		origin:  origin,
		visited: map[string]struct{}{},
	}
}

// fetchHTML downloads from a URL and parses to HTML DOM.
func fetchHTML(u *url.URL) (*html.Node, error) {
	resp, err := http.Get(u.String())
	if err != nil {
		fmt.Printf("Error fetching URL %q: %v\n", u, err)
		return nil, err
	}
	defer resp.Body.Close()
	return html.Parse(resp.Body)
}

// getURLAttr finds a named attribute of an HTML node and returns a reference to it.
func getAttr(n *html.Node, name string) *html.Attribute {
	for i, attr := range n.Attr {
		if attr.Key == name {
			return &n.Attr[i]
		}
	}
	return nil
}

// getURLAttr finds a named attribute of an HTML node and parses its value to a URL.
func getURLAttr(n *html.Node, name string) (*html.Attribute, *url.URL) {
	a := getAttr(n, name)
	if a == nil {
		return nil, nil
	}
	u, err := url.Parse(a.Val)
	if err != nil {
		log.Printf("Bad url: %q\n", a.Val)
		return nil, nil
	}
	return a, u
}

// relativize turns an fully-qualified URL into a relative URL.
func relativize(u *url.URL) {
	u.Scheme = ""
	u.Host = ""
}

// sortQueryValues sorts the values of all multi-valued query parameters.
func sortQueryValues(u *url.URL) {
	q := u.Query()
	for k, v := range q {
		sort.Strings(v)
		q[k] = v
	}
	u.RawQuery = q.Encode()
}

func (c Crawler) isLocal(u *url.URL) bool {
	return u.Hostname() == "" || strings.TrimPrefix(u.Hostname(), "www.") == strings.TrimPrefix(c.origin, "www.")
}

func isDynamicPage(u *url.URL) bool {
	path := u.Path
	// If there is an extension, it's a static resource
	parts := strings.Split(path, "/")
	return !strings.Contains(parts[len(parts)-1], ".")
}

func isHTMLContentType(s string) bool {
	t, _, _ := strings.Cut(s, ";")
	return s == "" || t == "text/html"
}

// staticate recursively parses an HTML document, excracting links to regular
// HTML documents on the origin site, and converting all URLs pointing to the
// origin site to relative form.
// TODO
//   - Find everything that has a link-like value
//   - If it's on our self-list, relativize it
//   - Always ignore images and other media
//   - Limit returned links to defined sub-page patterns
func (c *Crawler) staticate(n *html.Node, origin string) []*url.URL {
	links := []*url.URL{}
	if n.Type == html.ElementNode {
		// TODO: Prune nodes we don't want, e.g. <link rel="EditURI" ...>
		// TODO: Deal with data-* attributes
		switch n.DataAtom {
		case atom.A:
			a, u := getURLAttr(n, "href")
			if a == nil || u == nil || !c.isLocal(u) {
				break
			}
			// Follow
			if u.Path == "" {
				u.Path = "/"
			}
			oURL := *u
			links = append(links, &oURL)
			// Relativize
			relativize(u)
			a.Val = u.String()
		case atom.Img:
			// src
			a, u := getURLAttr(n, "src")
			if a != nil && u != nil && c.isLocal(u) {
				// Relativize
				relativize(u)
				a.Val = u.String()
			}
			// srcset
			a = getAttr(n, "srcset")
			if a == nil {
				break
			}
			srcs := strings.Split(a.Val, ",")
			for i, img := range srcs {
				var src, size string
				fmt.Sscanf(img, "%s %s", &src, &size)
				u, err := url.Parse(src)
				if err != nil {
					continue
				}
				if c.isLocal(u) {
					relativize(u)
				}
				srcs[i] = fmt.Sprintf("%s %s", u, size)
			}
			a.Val = strings.Join(srcs, ",")
			// Handle data-medium-file, data-large-file, data-permalink, data-orig-file.
			for _, d := range []string{"data-large-file", "data-medium-file", "data-orig-file", "data-permalink"} {
				a, u := getURLAttr(n, d)
				if a != nil && u != nil && c.isLocal(u) {
					// Relativize
					relativize(u)
					a.Val = u.String()
				}
			}
		case atom.Link: // href
			a, u := getURLAttr(n, "href")
			if a == nil || u == nil || !c.isLocal(u) {
				break
			}
			if isDynamicPage(u) {
				// Grab, but don't process or recurse into, dynamically-generated HTML-like (e.g RSS feed)
				c.saveRaw(*u)
			}
			relativize(u)
			a.Val = u.String()
		case atom.Script:
			// src
			a, u := getURLAttr(n, "src")
			if a != nil && u != nil && c.isLocal(u) {
				relativize(u)
				a.Val = u.String()
				break
			}

			// Slurp up all txt nodes in the script, frobnicate, and put back.
			var b strings.Builder
			for x := n.FirstChild; x != nil; x = n.FirstChild {
				b.WriteString(x.Data)
				n.RemoveChild(x)
			}
			// Frobnicate select URLs.
			js := b.String()
			// log.Println("Frobnicating JS. In:", js)
			for _, s := range STATIC_REPLACEMENTS {
				pattern := `https:\/\/` + origin + s
				js = strings.Replace(js, pattern, s, -1)
			}
			// log.Println("  Out:", js)
			n.AppendChild(&html.Node{Type: html.TextNode, Data: js})
			// TODO: Decide if there are URLs we need to extract from script for crawling, e.g. JSON data.
		case atom.Meta:
			// TODO: Decide if we should do something with these.
			// a, u := getURLAttr(n, "content")
		}
	}
	for x := n.FirstChild; x != nil; x = x.NextSibling {
		links = append(links, c.staticate(x, origin)...)
	}
	return links
}

// processURL fetches, parses and staticates a URL
// returning serialized (staticated) content and a list of further URLs to process.
func (c *Crawler) processURL(u *url.URL) (*resource.Resource, []*url.URL, error) {

	resp, err := c.httpClient.Get(u.String())
	if err != nil {
		fmt.Printf("Error fetching URL %q: %v\n", u, err)
		return nil, nil, err
	}
	defer resp.Body.Close()

	// TODO: Follow multiple redirects
	switch resp.StatusCode {
	case 301, 302, 303, 307, 308:
		loc := resp.Header.Get("Location")
		l, err := url.ParseRequestURI(loc)
		if err != nil {
			log.Printf("Redirect from %q to invalid url %q: %v\n", u, loc, err)
			return nil, nil, err
		}
		log.Printf("Saving redirect from %q to %q\n", u, loc)
		return &resource.Resource{Redirect: loc}, []*url.URL{l}, nil
	}

	r := &resource.Resource{ContentType: resp.Header.Get("Content-Type")}
	if !isHTMLContentType(r.ContentType) {
		r.Content, err = io.ReadAll(resp.Body)
		return r, nil, err
	}

	doc, err := html.Parse(resp.Body)

	if err != nil {
		log.Printf("Error fetching %q: %v\n", u, err)
		return nil, nil, err
	}
	links := c.staticate(doc, u.Hostname())
	content := new(bytes.Buffer)
	html.Render(content, doc)

	r.Content = content.Bytes()
	r.ContentType = "text/html"
	return r, links, nil
}

// followRedirects follows and saves a chain of redirects.
// If a non-redirect response is received from a local URL, the response
// is returned. In this case the caller MUST close the response body.
func (c *Crawler) followRedirects(u url.URL) (*url.URL, *http.Response) {
	redirCount := 0
	for {
		relativize(&u)
		sortQueryValues(&u)
		if _, ok := c.visited[u.String()]; ok {
			// Already visited
			return nil, nil
		}
		resp, err := c.httpClient.Get(u.String())
		if err != nil {
			fmt.Printf("Error fetching URL %q: %v\n", u.String(), err)
			return nil, nil
		}
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
			resp.Body.Close()
			loc := resp.Header.Get("Location")
			if redirCount > MAX_REDIRECTS {
				log.Printf("Too many redirects, last was %q to %q.\n", &u, loc)
				return nil, nil
			}
			l, err := url.ParseRequestURI(loc)
			if err != nil {
				log.Printf("Redirect from %q to invalid url %q: %v\n", &u, l, err)
				return nil, nil
			}

			log.Printf("Saving redirect from %q to %q\n", &u, l)
			if err := c.db.Write(u.String(), &resource.Resource{Redirect: loc}); err != nil {
				log.Printf("Error saving redirect from %q to %q: %v\n", &u, loc, err)
				return nil, nil
			}
			if !c.isLocal(l) {
				log.Printf("Redirect from %q to off-site url %q\n", &u, l)
				return nil, nil
			}
			u = *l
			redirCount++
		default:
			return &u, resp
		}
	}
}

// saveRaw saves the contents fetched from a URL without any processing.
// Use this for grabbing static contents of dynamically-generated non-HTML.
func (c *Crawler) saveRaw(u url.URL) {
	l, resp := c.followRedirects(u)
	if resp == nil {
		// No content found
		log.Printf("Got redirected to non-local content %q.\n", &u)
		return
	}
	defer resp.Body.Close()

	relativize(l)
	sortQueryValues(l)
	if _, ok := c.visited[l.String()]; ok {
		// Already visited
		return
	}

	rs := &resource.Resource{
		ContentType: resp.Header.Get("Content-Type"),
	}
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading response body from URL %q: %v\n", &u, err)
		return
	}
	rs.Content = content
	// url.URL.String() outputs querystrings in key-sorted order.
	if err := c.db.Write(l.String(), rs); err != nil {
		// TODO: Graceful error handling.
		log.Fatalf("Could not save raw content for %q: %v", l, err)
	}
}

func (c *Crawler) Crawl(u *url.URL, fetchLimit int) {
	// Set up
	startHost := u.Hostname()
	if u.Path == "" {
		u.Path = "/"
	}
	toVisit := []*url.URL{u}

	log.Println("Beginning crawl at:", u)

	// Start crawling.
	// TODO: Make this aware of site structure.
	fetched := 0
	for len(toVisit) > 0 && fetched < fetchLimit {
		// Find next link
		u := toVisit[0]
		toVisit = toVisit[1:]
		u.Fragment = ""
		if _, ok := c.visited[u.String()]; ok {
			// Already visited
			continue
		}
		c.visited[u.String()] = struct{}{}

		fetched++
		log.Printf("  Processing URL %d: %s\n", fetched, u)

		resource, links, err := c.processURL(u)
		if err != nil {
			log.Printf("Error fetching %q: %v\n", u, err)
			continue
		}
		// Add links to crawl (start site only)
		for _, u := range links {
			if u.Hostname() == startHost {
				toVisit = append(toVisit, u)
			}
		}

		// Write content to DB
		if err := c.db.Write(u.Path, resource); err != nil {
			// TODO: Graceful error handling.
			log.Fatalf("Could not save HTML content for %q: %v", u.Path, err)
		}
	}
	log.Println("Visited:", c.visited)
}

func (c *Crawler) CrawlNewResource(u *url.URL, conf *site.Config, fetchLimit int) error {
	// Set up
	var startHost string
	for _, d := range conf.Domains {
		if d == u.Hostname() {
			startHost = d
			continue
		}
	}

	if startHost == "" {
		return fmt.Errorf("resource %q is not in the domain list of the site config: %v", u.Hostname(), conf.Domains)
	}

	if u.Path == "" {
		u.Path = "/"
	}

	var rType string
	for _, r := range conf.Resources {
		re := regexp.MustCompile(r.Path)

		matches := re.FindStringSubmatch(u.Path)
		if matches == nil {
			continue
		}
		rType = r.Name
		log.Printf("Resource is of type: %s\n", rType)
		// TODO: Parse out the named capture groups into variables.
		break
	}
	if rType == "" {
		return fmt.Errorf("could not identify resource type from url: %s", u)
	}

	// visited := map[string]struct{}{}
	// toVisit := []*url.URL{u}

	log.Println("Crawling resource: ", u)

	return errors.New("CrawlNewResource not fully implemented")
}
