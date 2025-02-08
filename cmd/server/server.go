/*
 * A simple web server to serve a mix of staticated HTML from a database
 * and asset files from disk.
 */

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// URL prefixes to serve from assets in the filesystem. Others are blocked.
// These should also be synced to S3 (filtered by file type if possible).
var _DEFAULT_ASSET_PATHS = []string{
	"images",
	"img",
	"moives",
	"photos",
	"presentations",
	"sounds",
	"wp-content/plugins",
	"wp-content/themes",
	"wp-content/uploads",
	"wp-includes/css",
	"wp-includes/js",
}

var port = flag.Int("port", 8080, "TCP port to listen on.")
var assetRoot = flag.String("asset_root", "/var/www/html", "Local root of asset files.")
var assetPaths = flag.String("asset_paths", strings.Join(_DEFAULT_ASSET_PATHS, ","), "Allowed paths under the asset root to serve assets from.")
var dbPath = flag.String("db", "", "Database of staticated content.") // TODO: Make this a handler URI as used in polyester.go
var dbBucket = flag.String("bucket", "polyester", "BBolt bucket to read from.")

func handleAssetPaths() {
	for _, prefix := range strings.Split(*assetPaths, ",") {
		urlPrefix := fmt.Sprintf("/%s/", prefix)
		localDir := fmt.Sprintf("%s/%s", *assetRoot, prefix)
		http.Handle(urlPrefix, http.StripPrefix(urlPrefix, http.FileServer(http.Dir(localDir))))
	}
}

type ReopenableDB struct {
	dbPath string
	db     *bbolt.DB
	mu     sync.RWMutex
}

func (r *ReopenableDB) DB() *bbolt.DB {
	if r.db == nil {
		r.open()
	}
	r.mu.RLock()
	return r.db
}

func (r *ReopenableDB) Release() {
	r.mu.RUnlock()
}

func (r *ReopenableDB) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return
	}
	r.db.Close()
	r.db = nil
}

func (r *ReopenableDB) open() {
	db, err := bbolt.Open(r.dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second, ReadOnly: true})
	if err != nil {
		log.Printf("Error (re)opening database at %q: %v", r.dbPath, err)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	olddb := r.db
	r.db = db
	if olddb != nil {
		olddb.Close()
	}
}

type BBoltHandler struct {
	db *ReopenableDB
}

func NewBBoltHandler(dbPath string) *BBoltHandler {
	return &BBoltHandler{
		db: &ReopenableDB{dbPath: dbPath},
	}
}

func (b *BBoltHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Look up req.URL
	var html []byte
	path := req.URL.Path
	switch path {
	case "/statusz":
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("I am running.\r\nTODO: Put something useful here."))
		return
	case "/reloadz":
		log.Printf("Reopening database at %q", b.db.dbPath)
		b.db.open()
		http.Redirect(w, req, "/", http.StatusFound)
		return
	}

	err := func() error {
		// Get an RLocked handle on the database.
		db := b.db.DB()
		defer b.db.Release()
		return db.View(func(tx *bbolt.Tx) error {
			bkt := tx.Bucket([]byte(*dbBucket))
			val := bkt.Get([]byte(path))
			if val != nil {
				html = make([]byte, len(val))
				copy(html, val)
			}
			return nil
		})
	}()
	if err != nil {
		w.WriteHeader(500)
		return
	}
	if html == nil {
		log.Printf("Path %q not in db.\n", path)
		w.WriteHeader(404)
	}
	w.Header().Set("Content-Type", "text/html")
	if i, err := w.Write(html); i != len(html) || err != nil {
		log.Printf("Error writing response: %d/%d bytes, %v", i, len(html), err)
	}
}

func (b *BBoltHandler) Close() {
	b.db.Close()
}

// handlePolyesterPaths adds handlers to serve content from a database.
func handlePolyesterPaths(dbPath string) *BBoltHandler {
	h := NewBBoltHandler(dbPath)
	http.Handle("/", http.StripPrefix("", h))
	return h
}

func main() {
	flag.Parse()
	if *dbPath == "" {
		log.Fatal("Must specify a content database to open with --db= flag.")
	}
	log.SetOutput(os.Stderr)
	handleAssetPaths()

	polyHandler := handlePolyesterPaths(*dbPath)
	defer polyHandler.Close()

	log.Println("Starting server on port", *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}
