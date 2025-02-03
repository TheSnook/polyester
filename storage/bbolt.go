package storage

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/TheSnook/polyester/proto/resource"
	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

type BBoltStorage struct {
	db     *bbolt.DB
	bucket string
}

func newBBolt(path string) Storage {
	p := strings.Split(path, ":")
	if len(p) != 2 {
		// Error
		log.Fatalf(`BBolt path %q does not have expected format "<path>:<bucket>".`, path)
	}

	db, err := bbolt.Open(p[0], 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		log.Fatalf("Could not open database %q: %v", p[0], err)
	}

	db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(p[1]))
		if err != nil {
			return fmt.Errorf("create bucket %q: %s", p[1], err)
		}
		return nil
	})

	return &BBoltStorage{
		db:     db,
		bucket: p[1],
	}
}

func (s *BBoltStorage) Write(k string, r *resource.Resource) error {
	v, err := proto.Marshal(r)
	if err != nil {
		return err
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(s.bucket))
		err := b.Put([]byte(k), v)
		return err
	})
}

func (s *BBoltStorage) Close() {
	s.db.Close()
}

func init() {
	register("bbolt", newBBolt)
}
