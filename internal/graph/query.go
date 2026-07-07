package graph

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// scoreNodes ranks nodes by BM25 over (label + flattened attr values)
// against the query string. Returns nodes in descending score order.
//
// Uses the same tokenisation as internal/knowledge/search.go so query
// hits look the same across the corpus and the runtime graph. Pulled
// in-package to avoid the import cycle a shared util would create.
func scoreNodes(nodes map[string]Node, query string) []Node {
	const (
		k1     = 1.5
		b      = 0.75
		labelW = 2.0
	)
	qTokens := tokenize(query)
	if len(qTokens) == 0 {
		return nil
	}

	docs := make(map[string][]string, len(nodes))
	labels := make(map[string][]string, len(nodes))
	df := map[string]int{}
	totalLen := 0
	for id, n := range nodes {
		body := tokenize(flattenAttrs(n.Attrs)) // attr values
		label := tokenize(n.Label + " " + n.Kind)
		docs[id] = body
		labels[id] = label
		totalLen += len(body) + len(label)
		seen := map[string]bool{}
		for _, t := range body {
			if !seen[t] {
				df[t]++
				seen[t] = true
			}
		}
		for _, t := range label {
			if !seen[t] {
				df[t]++
				seen[t] = true
			}
		}
	}
	N := float64(len(nodes))
	avgLen := 1.0
	if N > 0 {
		avgLen = float64(totalLen) / N
		if avgLen == 0 {
			avgLen = 1
		}
	}

	type scored struct {
		node  Node
		score float64
	}
	hits := make([]scored, 0, len(nodes))
	for id, n := range nodes {
		score := 0.0
		bodyTF := termFreq(docs[id])
		labelTF := termFreq(labels[id])
		dl := float64(len(docs[id]) + len(labels[id]))
		for _, q := range qTokens {
			if df[q] == 0 {
				continue
			}
			idf := math.Log(1 + (N-float64(df[q])+0.5)/(float64(df[q])+0.5))
			tf := float64(bodyTF[q]) + float64(labelTF[q])*labelW
			if tf == 0 {
				continue
			}
			norm := tf * (k1 + 1) / (tf + k1*(1-b+b*dl/avgLen))
			score += idf * norm
		}
		if score > 0 {
			hits = append(hits, scored{node: n, score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]Node, len(hits))
	for i, h := range hits {
		out[i] = h.node
	}
	return out
}

// flattenAttrs concatenates string attr values into one searchable
// stream. Non-string values are coerced via toString so any JSON-able
// value can still match the query.
func flattenAttrs(attrs map[string]any) string {
	if len(attrs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		switch v := attrs[k].(type) {
		case string:
			b.WriteString(v)
		default:
			b.WriteString(toString(v))
		}
		b.WriteByte(' ')
	}
	return b.String()
}

func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
			continue
		}
		if cur.Len() > 0 {
			tok := cur.String()
			if len(tok) >= 2 && !stopwords[tok] {
				out = append(out, tok)
			}
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		tok := cur.String()
		if len(tok) >= 2 && !stopwords[tok] {
			out = append(out, tok)
		}
	}
	return out
}

func termFreq(ts []string) map[string]int {
	m := make(map[string]int, len(ts))
	for _, t := range ts {
		m[t]++
	}
	return m
}

var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true,
	"that": true, "this": true, "from": true, "into": true,
	"are": true, "was": true, "but": true, "not": true, "you": true,
}
