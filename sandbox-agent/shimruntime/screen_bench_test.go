package shimruntime

import (
	"fmt"
	"strconv"
	"testing"
)

// BenchmarkScreenOutput covers the screen retained by every PTY exec, even
// when its attached client is only forwarding bytes to another terminal.
// Short lines exercise scrolling, which dominates build logs and seq output.
func BenchmarkScreenOutput(b *testing.B) {
	var output []byte
	for i := 1; i <= 10000; i++ {
		output = strconv.AppendInt(output, int64(i), 10)
		output = append(output, '\r', '\n')
	}
	for _, size := range []struct{ cols, rows uint16 }{{80, 24}, {160, 60}} {
		b.Run(fmt.Sprintf("%dx%d", size.cols, size.rows), func(b *testing.B) {
			screen := newScreenBuffer(size.rows, size.cols, DefaultScrollbackLines)
			b.SetBytes(int64(len(output)))
			b.ReportAllocs()
			for b.Loop() {
				for p := output; len(p) > 0; {
					n := min(len(p), 32*1024)
					screen.write(p[:n])
					p = p[n:]
				}
			}
		})
	}
}
