/*
 * Fetches website content according to a set of rules and
 * stores a copy in a database with all links relativized.
 */

package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/url"
	"os"
	"runtime/trace"
	"strings"

	"github.com/TheSnook/polyester/crawler"
	"github.com/TheSnook/polyester/site"
	"github.com/TheSnook/polyester/storage"
)

// Config flags
var dbPath = flag.String("db", "", "Scheme and path to database of staticated content.")
var configFile = flag.String("site", "", "A YAML file defining site parameters for smart updates.")

// Action flags
var startURL = flag.String("url", "", "Root URL to fetch.")
var aliasDomains = flag.String("domains", "", "Comma-separated list of domains to consider local. Origin of --url is always included.")
var newResource = flag.String("new_resource", "", "URL of a newly-created resource (page, post, etc.) to fetch.")
var updateResource = flag.String("update_resource", "", "URL of an updated resource (page, post, etc.) to fetch.")
var deleteResource = flag.String("delete_resource", "", "URL of a resource (page, post, etc.) to remove from the database.")
var fetchLimit = flag.Int("limit", 1, "Max URLs to fetch.")
var maxParallel = flag.Int("parallel", 1, "Max concurrent fetches.")

// Development and debug flags
var traceFile = flag.String("trace", "", "Write a Go execution trace file.")

func main() {
	log.SetOutput(os.Stderr)
	flag.Parse()

	if *traceFile != "" {
		tf, err := os.OpenFile(*traceFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0664)
		if err != nil {
			log.Fatalf("Could not open trace file %q", *traceFile)
		}
		trace.Start(tf)
		defer trace.Stop()
	}

	var siteConfig *site.Config
	if *configFile != "" {
		siteConfig = mustLoadSiteConfig(*configFile)
		if j, err := json.MarshalIndent(siteConfig, "", "\t"); err == nil {
			log.Printf("Loaded site config for %q:\n%s\n", siteConfig.Name, j)
		}
	}

	if *dbPath == "" {
		log.Fatal("Flag --db is required")
	}
	db := storage.New(*dbPath)
	defer db.Close()

	aliasDomainStrings := strings.Split(*aliasDomains, ",")
	aliases := make([]string, len(aliasDomainStrings))
	for i, a := range aliasDomainStrings {
		u, err := url.Parse("http://" + a + "/")
		if err != nil {
			log.Fatalf("Alias does not look like a valid hostname %q\n", a)
		}
		aliases[i] = u.Host
	}

	if *startURL != "" {
		u, err := url.Parse(*startURL)
		if err != nil {
			log.Fatalf("Could not parse start url %q: %v\n", *startURL, err)
		}
		c := crawler.New(u.Hostname(), aliases, db)
		c.CrawlP(*u, *fetchLimit, *maxParallel)

		return
	}
	if *newResource != "" {
		u, err := url.Parse(*startURL)
		if err != nil {
			log.Fatalf("Could not parse resource url %q: %v\n", *startURL, err)
		}
		c := crawler.New(u.Hostname(), aliases, db)
		if err := c.CrawlNewResource(u, siteConfig, *fetchLimit); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *updateResource != "" {
		log.Fatalln("Updating resources is not yet implemented.")
	}
	if *deleteResource != "" {
		log.Fatalln("Deleting resources is not yet implemented.")
	}
	log.Fatalln("Nothing to do. Please specify --url or one of the --<new|update|delete>_resouce parameters.")
}

func mustLoadSiteConfig(path string) *site.Config {
	var siteConfig *site.Config
	yaml, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Could not open site config file %q: %v\n", path, err)
	}
	if siteConfig, err = site.Load(yaml); err != nil {
		log.Fatalf("Could not parse site config file %q: %v\n", path, err)
	}

	return siteConfig
}
