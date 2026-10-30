package canvas

import "math"

// Layout geometry. All values are integers so the rendered x/y/width/height
// always satisfy the JSON Canvas integer-coordinate contract. The layout is a
// concentric radial one: the seed sits at the origin and each hop occupies a
// ring of increasing radius. Viewers (Obsidian and the like) let the user
// rearrange, so the layout only needs to be readable and, above all,
// deterministic, so the same subgraph always serializes to a byte-identical,
// diffable .canvas.
const (
	nodeWidth  = 300
	nodeHeight = 180
	// BaseRadius is the radius of the first hop ring; ring h sits at baseRadius*h
	// unless a crowded ring needs more room (see ringRadius).
	baseRadius = 480
	// RingGap is the minimum spacing reserved per node around a ring, used to
	// grow a crowded ring's radius so cards do not pile on top of each other.
	ringGap = 120
)

// ringRadius is the radius of the hop-th ring holding count nodes: baseRadius*hop,
// widened when count nodes would not fit around that circumference without
// overlapping (circumference >= count*(nodeWidth+ringGap)).
func ringRadius(hop, count int) int {
	r := baseRadius * hop
	if minR := int(math.Round(float64(count) * float64(nodeWidth+ringGap) / (2 * math.Pi))); minR > r {
		r = minR
	}
	return r
}

// position returns the top-left (x, y) of the index-th of count nodes on the
// hop-th ring. Hop 0 (the seed) and a degenerate ring sit at the origin. The
// result is a pure function of (hop, index, count), rounded to integers, so the
// layout is fully deterministic. JSON Canvas positions a node by its top-left
// corner, so the ring point is offset by half the node size to center the card
// on the ring.
func position(hop, index, count int) (x, y int) {
	if hop == 0 || count == 0 {
		return -nodeWidth / 2, -nodeHeight / 2
	}
	radius := float64(ringRadius(hop, count))
	angle := 2 * math.Pi * float64(index) / float64(count)
	cx := radius * math.Cos(angle)
	cy := radius * math.Sin(angle)
	return int(math.Round(cx)) - nodeWidth/2, int(math.Round(cy)) - nodeHeight/2
}
