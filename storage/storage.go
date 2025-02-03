package storage

import (
	"log"
	"strings"

	"github.com/TheSnook/polyester/proto/resource"
)

type Storage interface {
	Write(k string, r *resource.Resource) error
	Close()
}

var registry map[string]constructor

// Factory to construct a back-end for a given target.
// The target should include a scheme and path, e.g.
//   - bbolt:</path/to/db.file>:<bucket>
//   - s3:<bucket>
func New(target string) Storage {
	scheme, path, ok := strings.Cut(target, ":")
	if !ok {
		log.Fatalf(`Storage path %q does not have expected format "<scheme>:<path>".`, target)
	}
	fn, ok := registry[scheme]
	if !ok {
		log.Fatalf("No storage handler found for scheme %q.", scheme)
	}
	return fn(path)
}

type constructor func(string) Storage

func register(scheme string, fn constructor) {
	if registry == nil {
		registry = make(map[string]constructor)
	}
	registry[scheme] = fn
}
