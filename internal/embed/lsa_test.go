package embed

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The Go transform must reproduce the Python training script's embeddings,
// otherwise online similarity scores would not match what the ranker was
// trained on. models/parity.json is written by tools/ranking/train_eval.py.
func TestParityWithPythonTraining(t *testing.T) {
	m, err := Load("../../models/lsa.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../models/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Texts      []string    `json:"texts"`
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	for i, text := range fx.Texts {
		got := m.Embed(text)
		want := fx.Embeddings[i]
		allZero := true
		for _, v := range want {
			if v != 0 {
				allZero = false
			}
		}
		if allZero {
			if got != nil {
				t.Errorf("%q: want nil (no known terms), got a vector", text)
			}
			continue
		}
		if len(got) != len(want) {
			t.Fatalf("%q: dim %d, want %d", text, len(got), len(want))
		}
		for d := range want {
			if math.Abs(float64(got[d])-want[d]) > 1e-4 {
				t.Fatalf("%q dim %d: go %.6f, python %.6f", text, d, got[d], want[d])
			}
		}
	}
}

func TestSimilarTextsAreCloser(t *testing.T) {
	m, err := Load("../../models/lsa.json")
	if err != nil {
		t.Fatal(err)
	}
	coffee1 := m.Embed("espresso roast and latte at my favorite cafe")
	coffee2 := m.Embed("the barista pulled a great espresso with crema")
	hiking := m.Embed("hiking the summit trail through the forest")
	if Cosine(coffee1, coffee2) <= Cosine(coffee1, hiking) {
		t.Fatalf("coffee/coffee %.3f should exceed coffee/hiking %.3f",
			Cosine(coffee1, coffee2), Cosine(coffee1, hiking))
	}
}

func TestMean(t *testing.T) {
	v := Mean([][]float32{{1, 0}, {0, 1}})
	if math.Abs(float64(v[0])-math.Sqrt2/2) > 1e-6 || math.Abs(float64(v[1])-math.Sqrt2/2) > 1e-6 {
		t.Fatalf("mean = %v", v)
	}
	if Mean(nil) != nil {
		t.Fatal("mean of nothing should be nil")
	}
}
