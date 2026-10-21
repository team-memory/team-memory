package embeddings

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"sync"
	"unicode"
)

// Fake is a deterministic provider for tests. Each word is hashed to a fixed
// random direction and a text's vector is the normalized sum of its words, so
// texts sharing words are similar. It can be switched to fail or to return
// wrong dimensions.
type Fake struct {
	model Model

	mu      sync.Mutex
	failErr error
	badDims bool
	calls   int
	hook    func()
}

// NewFake returns a fake provider for a model with the given name and dimensions.
func NewFake(name string, dims int) *Fake {
	return &Fake{model: Model{Name: name, Dimensions: dims}}
}

func (f *Fake) Model() Model { return f.model }

// SetFailure makes every call fail with err (nil restores success).
func (f *Fake) SetFailure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failErr = err
}

// SetWrongDimensions makes every call return vectors one dimension too long.
func (f *Fake) SetWrongDimensions(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.badDims = on
}

// SetHook runs fn at the start of every Embed call (nil removes it), letting
// tests change state while a request is "at the provider".
func (f *Fake) SetHook(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = fn
}

// Calls reports how many times Embed was called.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *Fake) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls++
	failErr, badDims, hook := f.failErr, f.badDims, f.hook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if failErr != nil {
		return nil, failErr
	}
	dims := f.model.Dimensions
	if badDims {
		dims++
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = FakeVector(t, dims)
	}
	if err := checkVectors(f.model, len(texts), out); err != nil {
		return nil, err
	}
	return out, nil
}

// FakeVector is the deterministic bag-of-words vector used by Fake.
func FakeVector(text string, dims int) []float32 {
	v := make([]float64, dims)
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, w := range words {
		h := fnv.New64a()
		_, _ = h.Write([]byte(w))
		seed := h.Sum64()
		for d := range v {
			// SplitMix64 step per dimension gives a stable pseudo-random sign/magnitude.
			seed += 0x9e3779b97f4a7c15
			z := seed
			z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
			z = (z ^ (z >> 27)) * 0x94d049bb133111eb
			z ^= z >> 31
			v[d] += float64(int64(z)) / math.MaxInt64
		}
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	out := make([]float32, dims)
	if norm == 0 {
		out[0] = 1
		return out
	}
	norm = math.Sqrt(norm)
	for i, x := range v {
		out[i] = float32(x / norm)
	}
	return out
}
