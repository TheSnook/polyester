package site

import (
	"bytes"

	yaml "gopkg.in/yaml.v3"
)

type Config struct {
	Name string
	// TODO: Split into domains-to-recurse-into and domains-to-relativize-links-to
	//       (E.g. don't recurse into the published static site, but do relativize any links to it)
	Domains   []string
	Resources []Resource
}

type Resource struct {
	Name     string
	Path     string
	Follow   []string
	Metadata []Metadata
	Related  []Resource
}

type Metadata struct {
	Var, Property string
}

func Load(in []byte) (*Config, error) {
	out := Config{}
	d := yaml.NewDecoder(bytes.NewReader(in))
	d.KnownFields(true)
	if err := d.Decode(&out); err != nil {
		return &Config{}, err
	}
	return &out, nil
}
