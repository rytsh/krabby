package llm

import (
	"errors"
	"strings"
	"testing"
)

func TestResponseLimitBoundsStreamAndFallback(t *testing.T) {
	for _, payload := range []string{
		strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n", 10),
		`{"choices":[{"message":{"content":"` + strings.Repeat("x", 200) + `"}}]}`,
	} {
		c := &Client{responseLimit: 100}
		if _, err := c.readStream(strings.NewReader(payload), func() {}); !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("response not bounded: %v", err)
		}
	}
	c := &Client{responseLimit: 100}
	result, err := c.readStream(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`), func() {})
	if err != nil || result.text != "ok" {
		t.Fatalf("small response: %+v %v", result, err)
	}
}
