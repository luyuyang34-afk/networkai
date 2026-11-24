// Package embed turns post text into dense vectors for semantic retrieval.
//
// The model is LSA (latent semantic analysis): a TF-IDF vector projected onto
// the top singular vectors of the corpus's TF-IDF matrix. It is trained
// offline by tools/ranking/train_eval.py and exported as JSON. This package
// reproduces the Python transform exactly, and a parity test enforces it.
//
// Why LSA rather than a neural sentence encoder: the build environment could
// not download pretrained models. The Embedder interface is what the rest of
// the system depends on, so swapping in a neural model changes one file.
package embed

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
)

// Embedder maps text to an L2-normalised vector, or nil if the text has no
// known terms (Elasticsearch's cosine similarity rejects zero vectors).
type Embedder interface {
	Embed(text string) []float32
	Dim() int
}

type LSA struct {
	vocab      map[string]int
	idf        []float64
	components [][]float64 // dim x vocab
}

var token = regexp.MustCompile(`[a-z0-9]+`)

func Load(path string) (*LSA, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Vocab      []string    `json:"vocab"`
		IDF        []float64   `json:"idf"`
		Components [][]float64 `json:"components"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(raw.Vocab) != len(raw.IDF) || len(raw.Components) == 0 || len(raw.Components[0]) != len(raw.Vocab) {
		return nil, fmt.Errorf("%s: inconsistent model shapes", path)
	}
	m := &LSA{vocab: make(map[string]int, len(raw.Vocab)), idf: raw.IDF, components: raw.Components}
	for i, w := range raw.Vocab {
		m.vocab[w] = i
	}
	return m, nil
}

func (m *LSA) Dim() int { return len(m.components) }

func (m *LSA) Embed(text string) []float32 {
	counts := map[int]int{}
	for _, w := range token.FindAllString(strings.ToLower(text), -1) {
		if i, ok := m.vocab[w]; ok {
			counts[i]++
		}
	}
	if len(counts) == 0 {
		return nil
	}
	// Sublinear TF-IDF, L2-normalised.
	tf := make(map[int]float64, len(counts))
	var norm float64
	for i, c := range counts {
		v := (1 + math.Log(float64(c))) * m.idf[i]
		tf[i] = v
		norm += v * v
	}
	norm = math.Sqrt(norm)
	// Project onto the components, then L2-normalise the result.
	out := make([]float64, len(m.components))
	var onorm float64
	for d, comp := range m.components {
		var s float64
		for i, v := range tf {
			s += comp[i] * v / norm
		}
		out[d] = s
		onorm += s * s
	}
	if onorm == 0 {
		return nil
	}
	onorm = math.Sqrt(onorm)
	res := make([]float32, len(out))
	for i, v := range out {
		res[i] = float32(v / onorm)
	}
	return res
}

// Cosine returns the cosine similarity of two L2-normalised vectors.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// Mean returns the L2-normalised mean of vectors (the user's taste vector).
func Mean(vecs [][]float32) []float32 {
	var dim int
	for _, v := range vecs {
		if len(v) > 0 {
			dim = len(v)
			break
		}
	}
	if dim == 0 {
		return nil
	}
	sum := make([]float64, dim)
	for _, v := range vecs {
		if len(v) != dim {
			continue
		}
		for i, x := range v {
			sum[i] += float64(x)
		}
	}
	var n float64
	for _, x := range sum {
		n += x * x
	}
	if n == 0 {
		return nil
	}
	n = math.Sqrt(n)
	out := make([]float32, dim)
	for i, x := range sum {
		out[i] = float32(x / n)
	}
	return out
}
