package claudecode

import "strings"

// draft reassembles one assistant message from MessageDisplay chunks. Hooks
// run in the background, so chunks can arrive out of order; each carries its
// index and the last is marked final.
type draft struct {
	chunks map[int]string
	last   int // index of the final chunk; -1 until it arrives
}

func newDraft() *draft {
	return &draft{chunks: map[int]string{}, last: -1}
}

// add records a chunk and reports the whole text once every chunk up to the
// final one is in.
func (d *draft) add(index int, delta string, final bool) (string, bool) {
	d.chunks[index] = delta
	if final {
		d.last = index
	}
	if d.last < 0 || len(d.chunks) < d.last+1 {
		return "", false
	}
	return d.text(), true
}

// text joins the chunks received so far, in order.
func (d *draft) text() string {
	max := d.last
	for i := range d.chunks {
		if i > max {
			max = i
		}
	}
	var b strings.Builder
	for i := 0; i <= max; i++ {
		b.WriteString(d.chunks[i])
	}
	return b.String()
}
