// T076a — Go-side reference implementation of the JS parseSSEChunks
// algorithm in internal/oncall/render/static/detail.js.
//
// The two MUST mirror each other byte-for-byte. The detail.js
// `parseSSEChunks` function carries a doc-comment cross-reference
// to this file so a future edit on either side is forced through PR
// review on both.
//
// Spec §0 binds "no node_modules" → no Node-based JS runner; this Go
// shape test catches algorithmic bugs (chunk-boundary buffering,
// frame splitting, event/data line dispatch) cheaply.
//
// Algorithm under test:
//
//   - Maintain a buffer string.
//   - On each chunk: append, then loop splitting at "\n\n".
//   - Each frame's lines are parsed: `event:` sets the event type
//     (default "message"); `data:` lines accumulate (joined by "\n"
//     when there are multiple); blank line terminates the frame.
//   - Trailing partial after the last "\n\n" is preserved.
//   - At EOF, the residual buffer is NOT dispatched (per SSE spec).
package unit

import (
	"strings"
	"testing"
)

// frame is the structured event yielded by the parser.
type frame struct {
	Event string
	Data  string
}

// sseParser mirrors the JS parseSSEChunks closure. parse(chunk)
// returns zero or more dispatched frames in arrival order.
type sseParser struct {
	buf string
}

func newSSEParser() *sseParser { return &sseParser{} }

func (p *sseParser) parse(chunk string) []frame {
	p.buf += chunk
	var out []frame
	for {
		idx := strings.Index(p.buf, "\n\n")
		if idx < 0 {
			return out
		}
		raw := p.buf[:idx]
		p.buf = p.buf[idx+2:]
		event := "message"
		var dataLines []string
		for _, line := range strings.Split(raw, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(line[len("event:"):])
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(line[len("data:"):]))
			}
		}
		out = append(out, frame{Event: event, Data: strings.Join(dataLines, "\n")})
	}
}

// residual returns the unparsed buffer (lets tests assert that EOF
// leaves a trailing partial frame undispatched).
func (p *sseParser) residual() string { return p.buf }

func TestSSE_SingleFrame_DefaultMessageEvent(t *testing.T) {
	p := newSSEParser()
	frames := p.parse("data: {\"pod\":\"foo\"}\n\n")
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if frames[0].Event != "message" {
		t.Errorf("event = %q, want message", frames[0].Event)
	}
	if frames[0].Data != `{"pod":"foo"}` {
		t.Errorf("data = %q, want %q", frames[0].Data, `{"pod":"foo"}`)
	}
}

func TestSSE_NamedCompleteEvent(t *testing.T) {
	p := newSSEParser()
	frames := p.parse("event: complete\ndata: {\"evicted\":1}\n\n")
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if frames[0].Event != "complete" {
		t.Errorf("event = %q, want complete", frames[0].Event)
	}
	if frames[0].Data != `{"evicted":1}` {
		t.Errorf("data = %q, want %q", frames[0].Data, `{"evicted":1}`)
	}
}

func TestSSE_UTF8MultiByteSplitAcrossChunks(t *testing.T) {
	// "héllo" — é is a 2-byte UTF-8 sequence (0xC3 0xA9). Splitting
	// the chunk in the middle of that sequence at the byte level
	// would corrupt the run. We split at a logical boundary (string
	// midpoint) since Go's `string` is byte-indexed but the test
	// re-frames the algorithm at a chunk-boundary.
	p := newSSEParser()
	a := "data: hé"
	b := "llo\n\n"
	frames := p.parse(a)
	if len(frames) != 0 {
		t.Errorf("frames after partial chunk = %d, want 0", len(frames))
	}
	frames = p.parse(b)
	if len(frames) != 1 {
		t.Fatalf("frames after second chunk = %d, want 1", len(frames))
	}
	if frames[0].Data != "héllo" {
		t.Errorf("data = %q, want hello", frames[0].Data)
	}
}

func TestSSE_ChunkBoundaryInDataPrefix(t *testing.T) {
	p := newSSEParser()
	// Cut "data: foo\n\n" in the middle of "data:" itself.
	frames := p.parse("dat")
	if len(frames) != 0 {
		t.Errorf("frames = %d, want 0", len(frames))
	}
	frames = p.parse("a: foo\n\n")
	if len(frames) != 1 {
		t.Fatalf("frames after second chunk = %d, want 1", len(frames))
	}
	if frames[0].Data != "foo" {
		t.Errorf("data = %q, want foo", frames[0].Data)
	}
}

func TestSSE_EmptyDataLineIsValid(t *testing.T) {
	p := newSSEParser()
	frames := p.parse("data:\n\n")
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if frames[0].Data != "" {
		t.Errorf("data = %q, want empty", frames[0].Data)
	}
}

func TestSSE_TwoFramesInOneChunk(t *testing.T) {
	p := newSSEParser()
	frames := p.parse("data: a\n\ndata: b\n\n")
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if frames[0].Data != "a" || frames[1].Data != "b" {
		t.Errorf("frames = %+v, want a, b", frames)
	}
}

func TestSSE_TrailingPartialFrameNotDispatched(t *testing.T) {
	p := newSSEParser()
	frames := p.parse("data: a\n\ndata: b\n") // missing the trailing blank line
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1 (only the closed frame)", len(frames))
	}
	if frames[0].Data != "a" {
		t.Errorf("frames[0].Data = %q, want a", frames[0].Data)
	}
	if r := p.residual(); r != "data: b\n" {
		t.Errorf("residual = %q, want %q (open partial frame preserved)", r, "data: b\n")
	}
}
