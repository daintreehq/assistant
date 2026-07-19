package markdown

import (
	"fmt"
	"strings"
	"testing"
)

var benchmarkRendered Rendered

func benchmarkMarkdown() string {
	section := "## Performance notes\n\n- Parse **structured markdown** safely.\n- Keep `inline code` readable.\n- Wrap prose at terminal-cell boundaries without corrupting Unicode: 🌳.\n\n"
	return strings.Repeat(section, 24)
}

func BenchmarkRenderCacheHit4KB(b *testing.B) {
	r := New(darkTheme())
	src := benchmarkMarkdown()
	benchmarkRendered = r.Render(src, 96, false)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkRendered = r.Render(src, 96, false)
	}
}

func BenchmarkRenderCacheMissSameWidth4KB(b *testing.B) {
	r := NewWithCacheSize(darkTheme(), 1)
	src := benchmarkMarkdown()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkRendered = r.Render(fmt.Sprintf("%s\n<!-- %d -->", src, i), 96, false)
	}
}
