package ui

import (
	"math"
	"strconv"
	"strings"
	"sync"
)

const (
	splashVectorSubX = 4
	splashVectorSubY = 8

	splashVectorStrokeRadius = 34.5
	splashVectorLeftEdge     = 200.7934
	splashVectorRightEdge    = 823.1866
	splashVectorTopY         = 269.738
	splashVectorBottomY      = 743.1317

	splashVectorEmptyCoverageThreshold    = 0.20
	splashVectorFullCoverageThreshold     = 0.84
	splashVectorQuadrantCoverageThreshold = 0.42
)

var (
	splashVectorScaleX = 44.0 / (splashVectorRightEdge - splashVectorLeftEdge)
	splashVectorScaleY = float64(splashVisibleHeight) / (splashVectorBottomY - splashVectorTopY)

	splashVectorCache struct {
		once   sync.Once
		frames [][][]splashCell
	}
)

type splashCell struct {
	glyph    rune
	coverage float64
}

type splashVectorPartKind int

const (
	splashVectorPrefix splashVectorPartKind = iota
	splashVectorArchLeft
	splashVectorArchRight
)

type splashVectorPart struct {
	name        rune
	kind        splashVectorPartKind
	fill        splashVectorPolygon
	mask        splashVectorStrokePath
	minProgress float64
	maxProgress float64
}

type splashVectorSample struct {
	row           int
	col           int
	quadrant      int
	x             float64
	y             float64
	contributions []splashVectorContribution
}

type splashVectorContribution struct {
	part     int
	progress float64
}

type splashVectorPolygon []splashPoint

type splashVectorStrokePath struct {
	points []splashPoint
	cumLen []float64
	total  float64
}

type splashPoint struct {
	x float64
	y float64
}

func splashVectorFrameRows(idx int) [][]splashCell {
	if idx < 0 {
		idx = 0
	}
	if idx >= SplashFrames {
		idx = SplashFrames - 1
	}
	splashVectorCache.once.Do(func() {
		splashVectorCache.frames = splashBuildVectorFrames()
	})
	return splashVectorCache.frames[idx]
}

func splashBuildVectorFrames() [][][]splashCell {
	parts := splashVectorParts()
	samples := splashVectorSamples(parts)
	splashVectorSetVisibleProgressWindows(parts, samples)
	frames := make([][][]splashCell, SplashFrames)
	for frame := 0; frame < SplashFrames; frame++ {
		frames[frame] = splashRasterizeVectorFrame(parts, samples, frame)
	}
	return frames
}

func splashVectorSetVisibleProgressWindows(parts []splashVectorPart, samples []splashVectorSample) {
	for i := range parts {
		parts[i].minProgress = 1
		parts[i].maxProgress = 0
	}
	for _, sample := range samples {
		for _, contribution := range sample.contributions {
			part := &parts[contribution.part]
			if contribution.progress < part.minProgress {
				part.minProgress = contribution.progress
			}
			if contribution.progress > part.maxProgress {
				part.maxProgress = contribution.progress
			}
		}
	}
	for i := range parts {
		if parts[i].minProgress > parts[i].maxProgress {
			parts[i].minProgress = 0
			parts[i].maxProgress = 1
		}
	}
}

func splashVectorParts() []splashVectorPart {
	return []splashVectorPart{
		{
			name: 'T',
			kind: splashVectorPrefix,
			fill: splashParseVectorPolygon(splashVectorLowresTrunkPath),
			mask: splashParseVectorStrokePath(splashVectorLowresTrunkMaskPath),
		},
		{
			name: 'L',
			kind: splashVectorPrefix,
			fill: splashParseVectorPolygon(splashVectorLowresLeftLegPath),
			mask: splashParseVectorStrokePath(splashVectorLowresLeftLegMaskPath),
		},
		{
			name: 'R',
			kind: splashVectorPrefix,
			fill: splashParseVectorPolygon(splashVectorLowresRightLegPath),
			mask: splashParseVectorStrokePath(splashVectorLowresRightLegMaskPath),
		},
		{
			name: 'C',
			kind: splashVectorArchLeft,
			fill: splashParseVectorPolygon(splashVectorLowresArchPath),
			mask: splashParseVectorStrokePath(splashVectorLowresArchMaskPath),
		},
		{
			name: 'C',
			kind: splashVectorArchRight,
			fill: splashParseVectorPolygon(splashVectorLowresArchPath),
			mask: splashParseVectorStrokePath(splashVectorLowresArchMaskPath),
		},
	}
}

func splashVectorSamples(parts []splashVectorPart) []splashVectorSample {
	samples := make([]splashVectorSample, 0, SplashWidth*splashVisibleHeight*splashVectorSubX*splashVectorSubY)
	for row := 0; row < splashVisibleHeight; row++ {
		for col := 0; col < SplashWidth; col++ {
			for subY := 0; subY < splashVectorSubY; subY++ {
				for subX := 0; subX < splashVectorSubX; subX++ {
					cellX := float64(col) + (float64(subX)+0.5)/float64(splashVectorSubX)
					cellY := float64(row) + (float64(subY)+0.5)/float64(splashVectorSubY)
					x, y := splashCellToVectorPoint(cellX, cellY)
					contributions := make([]splashVectorContribution, 0, 2)
					for partIndex, part := range parts {
						if !part.fill.contains(x, y) {
							continue
						}
						progress, distance := part.mask.closestProgress(x, y)
						if distance > splashVectorStrokeRadius {
							continue
						}
						contributions = append(contributions, splashVectorContribution{
							part:     partIndex,
							progress: progress,
						})
					}
					if len(contributions) == 0 {
						continue
					}
					samples = append(samples, splashVectorSample{
						row:           row,
						col:           col,
						quadrant:      splashSampleQuadrant(subX, subY),
						x:             x,
						y:             y,
						contributions: contributions,
					})
				}
			}
		}
	}
	return samples
}

func splashCellToVectorPoint(cellX, cellY float64) (float64, float64) {
	x := splashVectorLeftEdge + (cellX-3.0)/splashVectorScaleX
	y := splashVectorTopY + cellY/splashVectorScaleY
	return x, y
}

func splashSampleQuadrant(subX, subY int) int {
	if subY < splashVectorSubY/2 {
		if subX < splashVectorSubX/2 {
			return 0
		}
		return 1
	}
	if subX < splashVectorSubX/2 {
		return 2
	}
	return 3
}

func splashRasterizeVectorFrame(parts []splashVectorPart, samples []splashVectorSample, frame int) [][]splashCell {
	var counts [SplashHeight][SplashWidth]int
	var quadCounts [SplashHeight][SplashWidth][4]int
	for _, sample := range samples {
		covered := false
		for _, contribution := range sample.contributions {
			if splashVectorContributionVisible(parts[contribution.part], contribution, sample.x, sample.y, frame) {
				covered = true
				break
			}
		}
		if !covered {
			continue
		}
		counts[sample.row][sample.col]++
		quadCounts[sample.row][sample.col][sample.quadrant]++
	}

	rows := make([][]splashCell, SplashHeight)
	for row := range rows {
		rows[row] = make([]splashCell, SplashWidth)
		for col := range rows[row] {
			rows[row][col] = splashCellFromCoverage(counts[row][col], quadCounts[row][col])
		}
	}
	splashHintLowresLogo(rows)
	return rows
}

func splashHintLowresLogo(rows [][]splashCell) {
	// The source SVG edges are vector-true, but the terminal raster has a coarse,
	// fixed 48x18 grid. Snap long straight runs to exact cell columns and full-cell
	// bottoms so straight edges do not pick up one-cell anti-aliasing halos. Curves
	// and angled branches still come directly from the supersampled SVG.
	splashHintOuterArch(rows)
	splashHintTrunk(rows)
	splashHintInnerBranches(rows)
	splashClearLowresNoise(rows)
}

func splashHintOuterArch(rows [][]splashCell) {
	for _, row := range []int{7, 8} {
		splashSnapVectorRun(rows, row, 3, 7, '█', 1, 7)
		splashSnapVectorRun(rows, row, 43, 47, '█', 1, 42)
	}
	splashSnapVectorRun(rows, 9, 3, 7, '█', 1, 7)
	splashSnapVectorRun(rows, 9, 43, 47, '█', 1, 42)
}

func splashHintTrunk(rows [][]splashCell) {
	for row := 4; row < splashVisibleHeight; row++ {
		splashSnapVectorRun(rows, row, 23, 27, '█', 1, 22, 27)
	}
}

func splashHintInnerBranches(rows [][]splashCell) {
	for _, row := range []int{9, 10, 11} {
		splashSnapVectorRun(rows, row, 15, 19, '█', 1, 14, 19)
		splashSnapVectorRun(rows, row, 31, 35, '█', 1, 30, 35)
	}
	splashSnapVectorRun(rows, 12, 15, 19, '█', 1, 14, 19)
	splashSnapVectorRun(rows, 12, 31, 35, '█', 1, 30, 35)
}

func splashClearLowresNoise(rows [][]splashCell) {
	for _, hint := range []struct {
		row  int
		cols []int
	}{
		{7, []int{2, 7, 42, 47}},
		{8, []int{2, 7, 42, 47}},
		{9, []int{2, 7, 14, 19, 30, 35, 42, 47}},
		{10, []int{14, 19, 22, 27, 30, 35}},
		{11, []int{14, 19, 22, 27, 30, 35}},
		{12, []int{14, 19, 22, 27, 30, 35}},
	} {
		if hint.row < 0 || hint.row >= len(rows) {
			continue
		}
		for _, col := range hint.cols {
			if col >= 0 && col < len(rows[hint.row]) {
				rows[hint.row][col] = splashCell{}
			}
		}
	}
}

func splashSnapVectorRun(rows [][]splashCell, row, start, end int, glyph rune, coverage float64, clearCols ...int) {
	if row < 0 || row >= len(rows) {
		return
	}
	if !splashVectorRunVisible(rows[row], start, end, clearCols) {
		return
	}
	for col := start; col < end && col < len(rows[row]); col++ {
		if col >= 0 {
			rows[row][col] = splashCell{glyph: glyph, coverage: coverage}
		}
	}
	for _, col := range clearCols {
		if col >= 0 && col < len(rows[row]) {
			rows[row][col] = splashCell{}
		}
	}
}

func splashVectorRunVisible(row []splashCell, start, end int, clearCols []int) bool {
	for col := start; col < end && col < len(row); col++ {
		if col >= 0 && row[col].coverage > 0 {
			return true
		}
	}
	for _, col := range clearCols {
		if col >= 0 && col < len(row) && row[col].coverage > 0 {
			return true
		}
	}
	return false
}

func splashVectorContributionVisible(part splashVectorPart, contribution splashVectorContribution, x, y float64, frame int) bool {
	switch part.kind {
	case splashVectorArchLeft:
		head := 0.5 * splashVectorDrawProgress(frame, splashCanopyStartFrame, splashCanopyEndFrame)
		return splashVectorPrefixVisible(part.mask, contribution.progress, x, y, head)
	case splashVectorArchRight:
		start := splashCanopyStartFrame + splashCanopyRightStartDelay
		tail := 1 - 0.5*splashVectorDrawProgress(frame, start, splashCanopyEndFrame)
		return splashVectorSuffixVisible(part.mask, contribution.progress, x, y, tail)
	default:
		start, end := splashVectorPartTiming(part.name)
		if frame < start {
			return false
		}
		if frame >= end {
			return true
		}
		head := part.minProgress + splashVectorDrawProgress(frame, start, end)*(part.maxProgress-part.minProgress)
		return splashVectorPrefixVisible(part.mask, contribution.progress, x, y, head)
	}
}

func splashVectorPartTiming(part rune) (int, int) {
	switch part {
	case 'L':
		return splashLeftBranchStartFrame, splashLeftBranchEndFrame
	case 'R':
		return splashRightBranchStartFrame, splashRightBranchEndFrame
	default:
		return splashTrunkStartFrame, splashTrunkEndFrame
	}
}

func splashVectorPrefixVisible(path splashVectorStrokePath, progress, x, y, head float64) bool {
	if head <= 0 {
		return false
	}
	if progress <= head {
		return true
	}
	cap := path.pointAt(head)
	return splashDistance(x-cap.x, y-cap.y) <= splashVectorStrokeRadius
}

func splashVectorSuffixVisible(path splashVectorStrokePath, progress, x, y, tail float64) bool {
	if tail >= 1 {
		return false
	}
	if progress >= tail {
		return true
	}
	cap := path.pointAt(tail)
	return splashDistance(x-cap.x, y-cap.y) <= splashVectorStrokeRadius
}

func splashVectorDrawProgress(frame, start, end int) float64 {
	if end <= start || frame >= end {
		return 1
	}
	if frame < start {
		return 0
	}
	t := float64(frame-start+1) / float64(end-start+1)
	return splashCubicBezier(t, 0.33, 1, 0.68, 1)
}

func splashCubicBezier(x, x1, y1, x2, y2 float64) float64 {
	u := x
	for i := 0; i < 8; i++ {
		xEstimate := splashCubicBezierSample(u, x1, x2) - x
		if math.Abs(xEstimate) < 1e-7 {
			return splashCubicBezierSample(u, y1, y2)
		}
		derivative := splashCubicBezierDerivative(u, x1, x2)
		if math.Abs(derivative) < 1e-7 {
			break
		}
		u -= xEstimate / derivative
	}
	lo, hi := 0.0, 1.0
	u = x
	for i := 0; i < 20; i++ {
		estimate := splashCubicBezierSample(u, x1, x2)
		if math.Abs(estimate-x) < 1e-7 {
			break
		}
		if estimate < x {
			lo = u
		} else {
			hi = u
		}
		u = (lo + hi) / 2
	}
	return splashCubicBezierSample(u, y1, y2)
}

func splashCubicBezierSample(t, a1, a2 float64) float64 {
	inv := 1 - t
	return 3*inv*inv*t*a1 + 3*inv*t*t*a2 + t*t*t
}

func splashCubicBezierDerivative(t, a1, a2 float64) float64 {
	return 3*(1-t)*(1-t)*a1 + 6*(1-t)*t*(a2-a1) + 3*t*t*(1-a2)
}

func splashCellFromCoverage(count int, quadrants [4]int) splashCell {
	const total = splashVectorSubX * splashVectorSubY
	coverage := float64(count) / float64(total)
	if coverage < splashVectorEmptyCoverageThreshold {
		return splashCell{glyph: ' ', coverage: 0}
	}
	if coverage > splashVectorFullCoverageThreshold {
		return splashCell{glyph: '█', coverage: 1}
	}

	const quadrantTotal = (splashVectorSubX / 2) * (splashVectorSubY / 2)
	mask := 0
	maxQuadrant := 0
	for i, count := range quadrants {
		if count > quadrants[maxQuadrant] {
			maxQuadrant = i
		}
		if float64(count)/float64(quadrantTotal) >= splashVectorQuadrantCoverageThreshold {
			mask |= 1 << i
		}
	}
	if mask == 0 {
		mask = 1 << maxQuadrant
	}
	return splashCell{glyph: splashGlyphForQuadrants(mask), coverage: splashQuantizedAntiAliasCoverage(coverage)}
}

func splashQuantizedAntiAliasCoverage(coverage float64) float64 {
	if coverage < 0.5 {
		return 0.72
	}
	return 1
}

func splashGlyphForQuadrants(mask int) rune {
	switch mask {
	case 0:
		return ' '
	case 1:
		return '▘'
	case 2:
		return '▝'
	case 3:
		return '▀'
	case 4:
		return '▖'
	case 5:
		return '▌'
	case 6:
		return '▞'
	case 7:
		return '▛'
	case 8:
		return '▗'
	case 9:
		return '▚'
	case 10:
		return '▐'
	case 11:
		return '▜'
	case 12:
		return '▄'
	case 13:
		return '▙'
	case 14:
		return '▟'
	default:
		return '█'
	}
}

func splashParseVectorPolygon(d string) splashVectorPolygon {
	return splashVectorPolygon(splashFlattenSVGPath(d))
}

func splashParseVectorStrokePath(d string) splashVectorStrokePath {
	points := splashFlattenSVGPath(d)
	cumLen := make([]float64, len(points))
	for i := 1; i < len(points); i++ {
		cumLen[i] = cumLen[i-1] + splashDistance(points[i].x-points[i-1].x, points[i].y-points[i-1].y)
	}
	total := 0.0
	if len(cumLen) > 0 {
		total = cumLen[len(cumLen)-1]
	}
	return splashVectorStrokePath{points: points, cumLen: cumLen, total: total}
}

func splashFlattenSVGPath(d string) []splashPoint {
	tokens := splashSVGPathTokens(d)
	points := make([]splashPoint, 0, 96)
	var current, start splashPoint
	for i := 0; i < len(tokens); {
		token := tokens[i]
		i++
		switch token {
		case "M":
			current = splashPoint{x: splashParseFloat(tokens[i]), y: splashParseFloat(tokens[i+1])}
			start = current
			points = append(points, current)
			i += 2
		case "C":
			for i < len(tokens) && !splashIsSVGPathCommand(tokens[i]) {
				c1 := splashPoint{x: splashParseFloat(tokens[i]), y: splashParseFloat(tokens[i+1])}
				c2 := splashPoint{x: splashParseFloat(tokens[i+2]), y: splashParseFloat(tokens[i+3])}
				end := splashPoint{x: splashParseFloat(tokens[i+4]), y: splashParseFloat(tokens[i+5])}
				points = append(points, splashFlattenCubic(current, c1, c2, end)...)
				current = end
				i += 6
			}
		case "L":
			for i < len(tokens) && !splashIsSVGPathCommand(tokens[i]) {
				current = splashPoint{x: splashParseFloat(tokens[i]), y: splashParseFloat(tokens[i+1])}
				points = append(points, current)
				i += 2
			}
		case "Z", "z":
			if current != start {
				points = append(points, start)
				current = start
			}
		}
	}
	return points
}

func splashFlattenCubic(p0, p1, p2, p3 splashPoint) []splashPoint {
	const segments = 28
	points := make([]splashPoint, 0, segments)
	for i := 1; i <= segments; i++ {
		t := float64(i) / float64(segments)
		inv := 1 - t
		points = append(points, splashPoint{
			x: inv*inv*inv*p0.x + 3*inv*inv*t*p1.x + 3*inv*t*t*p2.x + t*t*t*p3.x,
			y: inv*inv*inv*p0.y + 3*inv*inv*t*p1.y + 3*inv*t*t*p2.y + t*t*t*p3.y,
		})
	}
	return points
}

func splashSVGPathTokens(d string) []string {
	tokens := make([]string, 0, 256)
	for i := 0; i < len(d); {
		c := d[i]
		if c == ' ' || c == '\n' || c == '\t' || c == ',' {
			i++
			continue
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		start := i
		i++
		for i < len(d) {
			c = d[i]
			if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || ((c == '-' || c == '+') && (d[i-1] == 'e' || d[i-1] == 'E')) {
				i++
				continue
			}
			break
		}
		tokens = append(tokens, d[start:i])
	}
	return tokens
}

func splashIsSVGPathCommand(token string) bool {
	return len(token) == 1 && strings.IndexByte("MmLlCcZz", token[0]) >= 0
}

func splashParseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func (p splashVectorPolygon) contains(x, y float64) bool {
	winding := 0
	for i := 0; i < len(p)-1; i++ {
		a, b := p[i], p[i+1]
		if a.y <= y {
			if b.y > y && splashIsLeft(a, b, x, y) > 0 {
				winding++
			}
		} else if b.y <= y && splashIsLeft(a, b, x, y) < 0 {
			winding--
		}
	}
	return winding != 0
}

func splashIsLeft(a, b splashPoint, x, y float64) float64 {
	return (b.x-a.x)*(y-a.y) - (x-a.x)*(b.y-a.y)
}

func (p splashVectorStrokePath) closestProgress(x, y float64) (float64, float64) {
	if len(p.points) == 0 || p.total <= 0 {
		return 0, math.Inf(1)
	}
	bestDistance := math.Inf(1)
	bestProgress := 0.0
	for i := 0; i < len(p.points)-1; i++ {
		a, b := p.points[i], p.points[i+1]
		dx, dy := b.x-a.x, b.y-a.y
		segmentLengthSquared := dx*dx + dy*dy
		t := 0.0
		if segmentLengthSquared > 0 {
			t = ((x-a.x)*dx + (y-a.y)*dy) / segmentLengthSquared
			if t < 0 {
				t = 0
			} else if t > 1 {
				t = 1
			}
		}
		px, py := a.x+t*dx, a.y+t*dy
		distance := splashDistance(x-px, y-py)
		if distance < bestDistance {
			bestDistance = distance
			segmentLength := p.cumLen[i+1] - p.cumLen[i]
			bestProgress = (p.cumLen[i] + t*segmentLength) / p.total
		}
	}
	return bestProgress, bestDistance
}

func (p splashVectorStrokePath) pointAt(progress float64) splashPoint {
	if len(p.points) == 0 {
		return splashPoint{}
	}
	if progress <= 0 || p.total <= 0 {
		return p.points[0]
	}
	if progress >= 1 {
		return p.points[len(p.points)-1]
	}
	target := progress * p.total
	for i := 0; i < len(p.points)-1; i++ {
		if p.cumLen[i+1] < target {
			continue
		}
		segmentLength := p.cumLen[i+1] - p.cumLen[i]
		t := 0.0
		if segmentLength > 0 {
			t = (target - p.cumLen[i]) / segmentLength
		}
		a, b := p.points[i], p.points[i+1]
		return splashPoint{x: a.x + (b.x-a.x)*t, y: a.y + (b.y-a.y)*t}
	}
	return p.points[len(p.points)-1]
}

const splashVectorTrunkMaskPath = "M510 918C510 918 511 372 511 372"

const splashVectorLeftLegMaskPath = "M395 693C395 666 430 522 304 492"

const splashVectorRightLegMaskPath = "M631.464 693C631.464 666 596.464 522 722.464 492"

const splashVectorArchMaskPath = "M227 651C228 622 216.194 529.114 250.892 460.963C296 388 403 343 510 303C635 341 739 411 766 444C793 477 803 600 800 641"

const splashVectorTrunkPath = "M537.5077 743.1317C537.5077 743.1317 486.5425 743.1317 486.5425 743.1317C484.4716 743.1317 482.7868 741.4469 482.7868 739.376C482.7868 739.376 482.7868 404.6624 482.7868 404.6624C482.7868 402.065 483.7345 399.5378 485.4895 397.6775C492.6499 390.0608 502.3024 386.27 511.99 386.27C521.6776 386.27 531.3652 390.0959 538.4905 397.7126C540.2455 399.608 541.1932 402.1352 541.1932 404.6975C541.1932 404.6975 541.1932 739.376 541.1932 739.376C541.1932 741.4469 539.5435 743.1317 537.4375 743.1317C537.4375 743.1317 537.5077 743.1317 537.5077 743.1317Z"

const splashVectorLeftLegPath = "M424.2751 592.9388C424.2751 592.9388 424.2751 688.3757 424.2751 688.3757C424.2751 690.4466 422.5903 692.1314 420.5194 692.1314C420.5194 692.1314 369.5542 692.1314 369.5542 692.1314C367.4833 692.1314 365.7985 690.4466 365.7985 688.3757C365.7985 688.3757 365.7985 593.009 365.7985 593.009C365.7985 572.3 354.742 553.2056 336.8059 542.816C336.8059 542.816 316.3426 530.9873 316.3426 530.9873C314.026 529.6535 312.3061 527.4773 311.569 524.915C310.8319 522.3527 310.1299 518.7374 310.1299 514.7711C310.1299 498.3443 321.397 483.2513 338.1748 479.285C340.702 478.6883 343.3696 479.1095 345.6511 480.4082C345.6511 480.4082 366.1495 492.2018 366.1495 492.2018C402.127 512.981 424.2751 551.3453 424.2751 592.9037C424.2751 592.9037 424.2751 592.9388 424.2751 592.9388Z"

const splashVectorRightLegPath = "M599.74 592.9388C599.74 592.9388 599.74 688.3757 599.74 688.3757C599.74 690.4466 601.4248 692.1314 603.4957 692.1314C603.4957 692.1314 654.4609 692.1314 654.4609 692.1314C656.5318 692.1314 658.2166 690.4466 658.2166 688.3757C658.2166 688.3757 658.2166 593.009 658.2166 593.009C658.2166 572.3 669.2731 553.2056 687.2092 542.816C687.2092 542.816 707.6725 530.9873 707.6725 530.9873C709.9891 529.6535 711.709 527.4773 712.4461 524.915C713.1832 522.3527 713.8852 518.7374 713.8852 514.7711C713.8852 498.3443 702.6181 483.2513 685.8403 479.285C683.3131 478.6883 680.6455 479.1095 678.364 480.4082C678.364 480.4082 657.8656 492.2018 657.8656 492.2018C621.8881 512.981 599.74 551.3453 599.74 592.9037C599.74 592.9037 599.74 592.9388 599.74 592.9388Z"

const splashVectorArchPath = "M823.1866 524.0726C823.1866 524.0726 823.1866 593.009 823.1866 593.009C823.1866 595.0799 821.5018 596.7647 819.4309 596.7647C819.4309 596.7647 768.4657 596.7647 768.4657 596.7647C766.3948 596.7647 764.71 595.0799 764.71 593.009C764.71 593.009 764.71 523.9673 764.71 523.9673C764.71 487.8494 745.4401 454.4342 714.1309 436.3928C714.1309 436.3928 562.5691 348.8885 562.5691 348.8885C531.2599 330.812 492.7201 330.812 461.4109 348.8885C461.4109 348.8885 309.8491 436.3928 309.8491 436.3928C278.5399 454.4693 259.27 487.8494 259.27 523.9673C259.27 523.9673 259.27 586.2698 259.27 586.2698C259.27 586.4804 259.27 586.7261 259.27 593.7812C259.27 595.8521 257.5852 597.5369 255.5143 597.5369C255.5143 597.5369 204.5491 597.5369 204.5491 597.5369C202.4782 597.5369 200.7934 595.8521 200.7934 593.7812C200.7934 593.7812 200.7934 524.0726 200.7934 524.0726C200.7934 466.9649 231.2602 414.2096 280.681 385.6733C280.681 385.6733 432.0673 298.2743 432.0673 298.2743C481.5232 269.738 542.4217 269.738 591.8776 298.2743C591.8776 298.2743 743.2288 385.6382 743.2288 385.6382C792.6847 414.1745 823.1515 466.9649 823.1515 524.0726C823.1515 524.0726 823.1866 524.0726 823.1866 524.0726Z"

const splashVectorLowresTrunkMaskPath = splashVectorTrunkMaskPath

const splashVectorLowresLeftLegMaskPath = splashVectorLeftLegMaskPath

const splashVectorLowresRightLegMaskPath = splashVectorRightLegMaskPath

const splashVectorLowresArchMaskPath = splashVectorArchMaskPath

const splashVectorLowresTrunkPath = splashVectorTrunkPath

const splashVectorLowresLeftLegPath = splashVectorLeftLegPath

const splashVectorLowresRightLegPath = splashVectorRightLegPath

const splashVectorLowresArchPath = splashVectorArchPath
