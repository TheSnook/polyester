package crawler

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

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
}

// TODO: Break up this class. The Crawler, a Crawl, and the resource processing should be separated.
type Crawler struct {
	db         storage.Storage
	httpClient *http.Client
	origin     string
	aliases    []string
	seen       map[string]struct{}
	muSeen     sync.Mutex
}

func noRedirects(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

func New(origin string, aliases []string, db storage.Storage) Crawler {
	return Crawler{
		db: db,
		httpClient: &http.Client{
			CheckRedirect: noRedirects,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // FIXME
			},
		},
		origin:  origin,
		aliases: aliases,
		seen:    map[string]struct{}{},
	}
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

// rootRelativeURL returns a root-relative URL string based on the passed URL
func rootRelativeURL(u url.URL) string {
	relativize(&u)
	return u.String()
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

func (c *Crawler) isLocal(u url.URL) bool {
	return u.Hostname() == "" || strings.TrimPrefix(u.Hostname(), "www.") == strings.TrimPrefix(c.origin, "www.")
}

func (c *Crawler) isSeen(u url.URL) bool {
	c.muSeen.Lock()
	defer c.muSeen.Unlock()
	_, ok := c.seen[u.String()]
	return ok
}

func (c *Crawler) markSeen(u url.URL) {
	c.muSeen.Lock()
	defer c.muSeen.Unlock()
	c.seen[u.String()] = struct{}{}
}

func isDynamicPage(u *url.URL) bool {
	path := u.Path
	// If there is an extension, treat it as an asset (already static)
	// TODO: Deal with PHP and other scripts (hidden by WordPress, but not other platforms).
	parts := strings.Split(path, "/")
	return !strings.Contains(parts[len(parts)-1], ".")
}

func isHTMLContentType(s string) bool {
	t, _, _ := strings.Cut(s, ";")
	return s == "" || t == "text/html"
}

// staticateDoc recursively parses an HTML document, excracting links to regular
// HTML documents on the origin site, and converting all URLs pointing to the
// origin site to relative form.
// TODO
//   - Find everything that has a link-like value
//   - If it's on our self-list, relativize it
//   - Always ignore images and other media
//   - Detect and save any dynamically-generated non-HTML where possible
//   - Limit returned links to defined sub-page patterns
func (c *Crawler) staticateDoc(root *html.Node, origin string) []url.URL {
	links := []url.URL{}
	links = append(links, c.staticateNode(root, origin)...)
	for x := range root.Descendants() {
		links = append(links, c.staticateNode(x, origin)...)
	}
	return links
}

// staticateDoc recursively parses an HTML document, excracting links to regular
func (c *Crawler) staticateNode(n *html.Node, origin string) []url.URL {
	links := []url.URL{}

	if n.Type == html.CommentNode {
		// This deals with conditional comments containing links (e.g. to CSS)
		// and also obscures the original domain in regular comments.
		// FIXME: These might be resources we need to scrape and save.
		n.Data = strings.Replace(n.Data, "https://"+origin+"/", "/", -1)
		n.Data = strings.Replace(n.Data, "http://"+origin+"/", "/", -1)
		return nil
	}
	if n.Type != html.ElementNode {
		return nil
	}
	// TODO: Prune nodes we don't want, e.g. <link rel="EditURI" ...>
	// TODO: Deal with data-* attributes
	switch n.DataAtom {
	case atom.A:
		a, u := getURLAttr(n, "href")
		if a == nil || u == nil || !c.isLocal(*u) {
			log.Printf("  Skipping invalid/non-local link %q", u)
			break
		}
		if u.Path == "" && u.Host == "" && u.RawQuery == "" {
			// Fragment reference to current page or empty URL. No follow.
			log.Printf("  Skipping fragment-only link %q", u)
			break
		}

		// Follow
		if isDynamicPage(u) {
			// Only things that don't look like static assets get crawled.
			oURL := *u
			links = append(links, oURL)
		} else {
			log.Printf("  Skipping link that looks like a static asset %q", u)
		}
		// Relativize
		relativize(u)
		a.Val = u.String()
	case atom.Img:
		// src
		a, u := getURLAttr(n, "src")
		if a != nil && u != nil && c.isLocal(*u) {
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
			if c.isLocal(*u) {
				relativize(u)
			}
			srcs[i] = fmt.Sprintf("%s %s", u, size)
		}
		a.Val = strings.Join(srcs, ",")
		// Handle data-medium-file, data-large-file, data-permalink, data-orig-file.
		for _, d := range []string{"data-large-file", "data-medium-file", "data-orig-file", "data-permalink"} {
			a, u := getURLAttr(n, d)
			if a != nil && u != nil && c.isLocal(*u) {
				// Relativize
				relativize(u)
				a.Val = u.String()
			}
		}
	case atom.Link: // href
		break // FIXME
		a, u := getURLAttr(n, "href")
		if a == nil || u == nil || !c.isLocal(*u) {
			break
		}
		if isDynamicPage(u) {
			// Grab, but don't process or recurse into, dynamically-generated HTML-like (e.g RSS feed)
			c.saveRaw(*u)
		}
		relativize(u)
		a.Val = u.String()
	case atom.Script:
		break // FIXME
		// src
		a, u := getURLAttr(n, "src")
		if a != nil && u != nil && c.isLocal(*u) {
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
		break // FIXME
		// TODO: Decide if we should do something more with these.
		a, u := getURLAttr(n, "content")
		if a != nil && u != nil && c.isLocal(*u) {
			relativize(u)
			a.Val = u.String()
			break
		}
	case atom.Form:
		// We "defang" these for now.
		// TODO: Conditionally allow local <form> submits to support smart edge routing.
		a, u := getURLAttr(n, "content")
		if a != nil && u != nil && c.isLocal(*u) {
			a.Val = "#"
		}
	}

	return links
}

// processURL fetches, parses and staticates a URL
// returning serialized (staticated) content and a list of further URLs to process.
func (c *Crawler) processURL(u url.URL) (*resource.Resource, []url.URL, error) {

	resp, err := c.httpClient.Get(u.String())
	if err != nil {
		fmt.Printf("Error fetching URL %q: %v\n", &u, err)
		return nil, nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 301, 302, 303, 307, 308:
		loc := resp.Header.Get("Location")
		l, err := url.ParseRequestURI(loc)
		if err != nil {
			log.Printf("Redirect from %q to invalid url %q: %v\n", &u, loc, err)
			return nil, nil, err
		}
		log.Printf("Found redirect from %q to %q\n", &u, loc)
		return &resource.Resource{Redirect: loc}, []url.URL{*l}, nil
	}

	// Generated non-HTML resources get saved un-parsed.
	// FIXME: Handle some special content types. E.g. generated CSS with image links.
	r := &resource.Resource{ContentType: resp.Header.Get("Content-Type")}
	if !isHTMLContentType(r.ContentType) {
		r.Content, err = io.ReadAll(resp.Body)
		return r, nil, err
	}

	doc, err := html.Parse(resp.Body)
	if err != nil {
		log.Printf("Error parsing HTML from %q: %v\n", &u, err)
		return nil, nil, err
	}

	// Convert the document to a static-compatible form with fully
	// relative links, and extract links to other documents in the site.
	links := c.staticateDoc(doc, u.Hostname())
	content := new(bytes.Buffer)
	html.Render(content, doc)
	r.Content = content.Bytes()

	return r, links, nil
}

// followRedirects follows and saves a chain of redirects.
// If a non-redirect response is received from a local URL, the response
// is returned. In this case the caller MUST close the response body.
func (c *Crawler) followRedirects(u url.URL) (*url.URL, *http.Response) {
	redirCount := 0
	for {
		sortQueryValues(&u)
		if c.isSeen(u) {
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
			if c.isLocal(*l) {
				log.Printf("Saving redirect from %q to %q\n", &u, l)
				if err := c.db.Write(rootRelativeURL(u), &resource.Resource{Redirect: rootRelativeURL(*l)}); err != nil {
					log.Printf("Error saving redirect from %q to %q: %v\n", &u, loc, err)
					return nil, nil
				}
			} else {
				log.Printf("Saving redirect from %q to off-site url %q\n", &u, l)
				if err := c.db.Write(rootRelativeURL(u), &resource.Resource{Redirect: loc}); err != nil {
					log.Printf("Error saving redirect from %q to %q: %v\n", &u, loc, err)
					return nil, nil
				}
				return l, nil
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
	log.Printf("    Attempting to save raw content of %q.\n", &u)
	l, resp := c.followRedirects(u)
	if resp == nil {
		// No content found
		log.Printf("Could not fech non-HTML dynamic content from %q.\n", &u)
		return
	}
	defer resp.Body.Close()

	relativize(l)
	sortQueryValues(l)
	if c.isSeen(*l) {
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

// CrawlP starts at a URL `u` and fetches up to `fetchLimit` URLs
// found by following links in each downloaded HTML page.
// Up to `maxP` page fetches are run concurrently.
func (c *Crawler) CrawlP(u url.URL, fetchLimit int, maxP int) {

	type result struct {
		key      string             // The site-relative URL fetched.
		resource *resource.Resource // The HTML or other content.
		links    []url.URL          // Local (site-relative), non-static links found.
		err      error              // Any error seen during fetching or parsing.
	}

	// The job queue
	toDoCond := sync.NewCond(&sync.Mutex{})
	toDo := []url.URL{}
	// Increment any time something is added to toDo
	// TODO: Wrap all this in a function.
	fetched := 0

	// Close this to shut down any goroutines when the craw is finished.
	done := make(chan struct{})

	// WaitGroup for pending unprocessed URLs. Incremented before URLs are
	// added to `toDo` so that the crawl doesn't stop prematurely during a
	// moment of idleness.
	wg := sync.WaitGroup{}

	// Results coming back from workers.
	results := make(chan result)

	// Links we found, but which exceeded fetchLimit, in string format. For tracking only.
	extraLinks := map[string]struct{}{}

	// The dispatcher takes URLs from the toDo queue and starts workers to process them.
	// Only `maxP` workers are run concurrently.
	dispatcher := func() {
		// A semaphore to control the concurrecy level.
		// TODO: Investigate whether it works better to control concurrency level
		//       only on HTTP fetches (or have a different concurrency level for each)
		sem := make(chan struct{}, maxP)
		for {
			select {
			case <-done:
				log.Println("Dispatcher: shutting down")
				return
			default:
				toDoCond.L.Lock()
				for len(toDo) == 0 {
					toDoCond.Wait()
				}
				// There's work to do!
				u := toDo[0]
				toDo = toDo[1:]
				toDoCond.L.Unlock()
				log.Printf("Dispatcher: attempting to start worker for %q", u.String())
				// Wait until we have enough parallel capaicty to do the work.
				sem <- struct{}{}
				go func(u url.URL) {
					log.Printf("Worker: Processing %q", u.String())
					res, links, err := c.processURL(u)
					log.Printf("Worker: Returning results for %q", u.String())
					results <- result{key: u.String(), resource: res, links: links, err: err}
					log.Printf("Worker: Results for %q returned", u.String())
					<-sem // Release semaphore
				}(u)
			}
		}
	}

	// Result processor
	resultProcessor := func() {
		for resp := range results {
			log.Printf("Picking up response for %q", resp.key)
			if resp.err != nil {
				log.Printf("Error processing URL %q: %v\n", resp.key, resp.err)
				// TODO: Put back on the processing queue and keep a retry count to
				//       deal with transient errors.
				wg.Done()
				continue
			}

			// Add any unique new URLs, up to fetchLimit
			toDoCond.L.Lock()
			for _, u := range resp.links {
				// Normalize
				if u.Path == "" {
					u.Path = "/"
				}
				u.Fragment = ""

				// Check if it's a viable candidate
				if !c.isLocal(u) || c.isSeen(u) {
					continue
				}

				// Check if we exceeded the provided limit
				if fetched >= fetchLimit {
					extraLinks[u.String()] = struct{}{}
					continue
				}

				// Create a job to scrape this URL
				wg.Add(1)
				c.markSeen(u)
				toDo = append(toDo, u)
				fetched++
			}
			toDoCond.L.Unlock()
			// Let the dispatcher know there is new work.
			toDoCond.Broadcast()

			// Write content to DB
			if err := c.db.Write(resp.key, resp.resource); err != nil {
				// TODO: Graceful error handling.
				log.Fatalf("Could not save HTML content for %q: %v", u.Path, err)
			}

			// Mark one response as done.
			wg.Done()
		}
	}

	enqueueUrl := func(u url.URL) {
		toDoCond.L.Lock()
		wg.Add(1)
		c.markSeen(u)
		toDo = append(toDo, u)
		fetched++
		toDoCond.L.Unlock()
		toDoCond.Signal()
	}

	// Start up our async workers
	go dispatcher()
	go resultProcessor()

	// Start the initial fetch.
	if u.Path == "" {
		u.Path = "/"
	}
	enqueueUrl(u)

	// URLs found during the crawll cause wg.Add(1) to be called.
	// Done() is called after processing, and only after any new URLs have been
	// added to the queue. This prevents premature shutdown on a temporarily
	// empty processing queue.
	wg.Wait()
	close(done)
	close(results)

	visited := make([]string, len(c.seen))
	i := 0
	for u := range c.seen {
		visited[i] = u
		i++
	}

	log.Printf("Visited [%d]: %s\n", len(visited), visited)
	log.Printf("Found but unvisited [%d]\n", len(extraLinks))
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
