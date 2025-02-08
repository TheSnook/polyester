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

	"github.com/TheSnook/polyester/crawler"
	"github.com/TheSnook/polyester/site"
	"github.com/TheSnook/polyester/storage"
)

// Config flags
var dbPath = flag.String("db", "", "Scheme and path to database of staticated content.")
var configFile = flag.String("site", "", "A YAML file defining site parameters for smart updates.")

// Action flags
var startURL = flag.String("url", "", "Root URL to fetch.")
var newResource = flag.String("new_resource", "", "URL of a newly-created resource (page, post, etc.) to fetch.")
var updateResource = flag.String("update_resource", "", "URL of an updated resource (page, post, etc.) to fetch.")
var deleteResource = flag.String("delete_resource", "", "URL of a resource (page, post, etc.) to remove from the database.")
var fetchLimit = flag.Int("limit", 1, "Max URLs to fetch.")

func main() {
	flag.Parse()
	log.SetOutput(os.Stderr)

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

	if *startURL != "" {
		u, err := url.Parse(*startURL)
		if err != nil {
			log.Fatalf("Could not parse start url %q: %v\n", *startURL, err)
		}
		c := crawler.New(u.Hostname(), db)
		c.Crawl(u, *fetchLimit)
		return
	}
	if *newResource != "" {
		u, err := url.Parse(*startURL)
		if err != nil {
			log.Fatalf("Could not parse resource url %q: %v\n", *startURL, err)
		}
		c := crawler.New(u.Hostname(), db)
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
