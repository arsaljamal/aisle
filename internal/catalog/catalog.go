// Package catalog reads the public WANDS dataset (Wayfair, MIT licence):
// products, real shopper queries and human relevance labels.
package catalog

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Product struct {
	ID          int64
	Name        string
	Class       string
	Category    string
	Description string
	Features    string
	Rating      float32
	Reviews     int32
}

// Document is the text a product is indexed under, for both keyword and
// vector search. Names come first because they carry the most signal.
func (p Product) Document(maxDesc int) string {
	desc := p.Description
	if len(desc) > maxDesc {
		desc = desc[:maxDesc]
	}
	return strings.Join([]string{p.Name, p.Class, p.Category, desc}, ". ")
}

type Query struct {
	ID    int
	Text  string
	Class string
}

// Gain is how relevant a product is to a query: Exact 2, Partial 1, else 0.
type Labels map[int]map[int64]int

func Gain(label string) int {
	switch label {
	case "Exact":
		return 2
	case "Partial":
		return 1
	}
	return 0
}

func open(dir, name string) (*csv.Reader, io.Closer, error) {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return nil, nil, fmt.Errorf("%w (run `make data` first)", err)
	}
	r := csv.NewReader(f)
	r.Comma = '\t'
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	if _, err := r.Read(); err != nil { // header
		f.Close()
		return nil, nil, err
	}
	return r, f, nil
}

func field(rec []string, i int) string {
	if i < len(rec) {
		return strings.TrimSpace(rec[i])
	}
	return ""
}

// EachProduct streams products so the 87 MB file is never held in memory.
func EachProduct(dir string, fn func(Product) error) error {
	r, c, err := open(dir, "product.csv")
	if err != nil {
		return err
	}
	defer c.Close()
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(field(rec, 0), 10, 64)
		if err != nil {
			continue
		}
		rating, _ := strconv.ParseFloat(field(rec, 7), 32)
		reviews, _ := strconv.ParseFloat(field(rec, 8), 32)
		p := Product{
			ID: id, Name: field(rec, 1), Class: field(rec, 2), Category: field(rec, 3),
			Description: field(rec, 4), Features: field(rec, 5),
			Rating: float32(rating), Reviews: int32(reviews),
		}
		if err := fn(p); err != nil {
			return err
		}
	}
}

func Queries(dir string) ([]Query, error) {
	r, c, err := open(dir, "query.csv")
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var out []Query
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		id, err := strconv.Atoi(field(rec, 0))
		if err != nil {
			continue
		}
		out = append(out, Query{ID: id, Text: field(rec, 1), Class: field(rec, 2)})
	}
}

func LoadLabels(dir string) (Labels, error) {
	r, c, err := open(dir, "label.csv")
	if err != nil {
		return nil, err
	}
	defer c.Close()
	out := Labels{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		q, err1 := strconv.Atoi(field(rec, 1))
		p, err2 := strconv.ParseInt(field(rec, 2), 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		if out[q] == nil {
			out[q] = map[int64]int{}
		}
		out[q][p] = Gain(field(rec, 3))
	}
}
