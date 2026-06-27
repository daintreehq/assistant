package ui

import (
	"math"
	"strings"
)

// The splash animation frames, maintained directly as Go.

// SPLASH_FRAMES: 20 source frames, each 18 lines of 48 columns using U+2588 (█)
// blocks and spaces. The mark draws
// itself in: trunk grows up (0-3), roots (4-6), then the canopy arch on 45° diagonals
// up to the centre peak that connects last (7-19); frame 19 is the complete logo.
//
// The visible pixels come from splashFinalFrame, a 14-row terminal raster of the real
// Daintree SVG using Unicode quadrant/half/full block glyphs. That keeps the historical
// 18-row boot envelope while compensating for xterm/WebGL's tall character cells and
// preserving the logo's curved silhouette.

func splashFrameLines(idx int) []string {
	finalFrame := splashFinalFrameLines()
	finalParts := splashFinalPartLines()
	geometry := splashRevealGeometryFor(finalParts)
	out := make([]string, SplashHeight)
	for y := range out {
		dst := []rune(splashBlankLine())
		finalRunes := []rune(finalFrame[y])
		partRunes := []rune(finalParts[y])
		for x := 0; x < SplashWidth; x++ {
			if splashPartRevealedAt(y, x, partRunes[x], idx, geometry) && splashIsInk(finalRunes[x]) {
				dst[x] = finalRunes[x]
			}
		}
		out[y] = string(dst)
	}
	return out
}

func splashPartRevealedAt(row, col int, part rune, frame int, geometry splashRevealGeometry) bool {
	switch part {
	case 'C':
		return splashCanopyRevealedAt(row, col, frame, geometry)
	case 'L':
		return splashStemRevealedAt('L', row, col, frame, splashLeftBranchStartFrame, splashLeftBranchEndFrame, geometry)
	case 'R':
		return splashStemRevealedAt('R', row, col, frame, splashRightBranchStartFrame, splashRightBranchEndFrame, geometry)
	case 'T':
		return splashStemRevealedAt('T', row, col, frame, splashTrunkStartFrame, splashTrunkEndFrame, geometry)
	default:
		return false
	}
}

func splashStemRevealedAt(part rune, row, col, frame, start, end int, geometry splashRevealGeometry) bool {
	if frame < start {
		return false
	}
	if frame >= end {
		return true
	}
	path, ok := geometry.stems[part]
	if !ok {
		return false
	}
	progress := splashStemProgress(part, frame, start, end)
	cellProgress, ok := path.cellProgress(row, col, splashStemRevealLead)
	return ok && cellProgress <= progress
}

type splashRevealGeometry struct {
	stems       map[rune]splashPathMetric
	canopyLeft  splashPathMetric
	canopyRight splashPathMetric
}

type splashPathPoint struct {
	row      int
	centerX  float64
	centerY  float64
	distance float64
	tangentX float64
	tangentY float64
}

type splashPathMetric struct {
	pointsByRow map[int]splashPathPoint
	total       float64
}

const (
	splashStemRevealLead   = 0.75
	splashCanopyRevealLead = 0.75
)

func splashRevealGeometryFor(lines []string) splashRevealGeometry {
	return splashRevealGeometry{
		stems: map[rune]splashPathMetric{
			'L': splashPathMetricFor(lines, func(part rune, _ int) bool { return part == 'L' }),
			'R': splashPathMetricFor(lines, func(part rune, _ int) bool { return part == 'R' }),
			'T': splashPathMetricFor(lines, func(part rune, _ int) bool { return part == 'T' }),
		},
		canopyLeft:  splashPathMetricFor(lines, func(part rune, col int) bool { return part == 'C' && col <= SplashWidth/2 }),
		canopyRight: splashPathMetricFor(lines, func(part rune, col int) bool { return part == 'C' && col > SplashWidth/2 }),
	}
}

func splashPathMetricFor(lines []string, include func(part rune, col int) bool) splashPathMetric {
	points := make([]splashPathPoint, 0, SplashHeight)
	for y, line := range lines {
		var sumX float64
		var cells int
		for x, r := range []rune(line) {
			if !include(r, x) {
				continue
			}
			sumX += float64(x) + 0.5
			cells++
		}
		if cells == 0 {
			continue
		}
		points = append(points, splashPathPoint{
			row:     y,
			centerX: sumX / float64(cells),
			centerY: float64(y) + 0.5,
		})
	}
	for i, j := 0, len(points)-1; i < j; i, j = i+1, j-1 {
		points[i], points[j] = points[j], points[i]
	}
	for i := 1; i < len(points); i++ {
		points[i].distance = points[i-1].distance + splashDistance(
			points[i].centerX-points[i-1].centerX,
			points[i].centerY-points[i-1].centerY,
		)
	}
	for i := range points {
		var dx, dy float64
		switch {
		case len(points) == 1:
			dx, dy = 0, -1
		case i == len(points)-1:
			dx = points[i].centerX - points[i-1].centerX
			dy = points[i].centerY - points[i-1].centerY
		default:
			dx = points[i+1].centerX - points[i].centerX
			dy = points[i+1].centerY - points[i].centerY
		}
		points[i].tangentX, points[i].tangentY = splashUnitVector(dx, dy)
	}
	if len(points) == 0 {
		return splashPathMetric{pointsByRow: make(map[int]splashPathPoint)}
	}
	metric := splashPathMetric{pointsByRow: make(map[int]splashPathPoint), total: points[len(points)-1].distance}
	for _, point := range points {
		metric.pointsByRow[point.row] = point
	}
	return metric
}

func (m splashPathMetric) cellProgress(row, col int, revealLead float64) (float64, bool) {
	point, ok := m.pointsByRow[row]
	if !ok {
		return 0, false
	}
	if m.total <= 0 {
		return 0, true
	}
	cellX := float64(col) + 0.5
	cellY := float64(row) + 0.5
	distance := point.distance + (cellX-point.centerX)*point.tangentX + (cellY-point.centerY)*point.tangentY - revealLead
	if distance < 0 {
		distance = 0
	} else if distance > m.total {
		distance = m.total
	}
	return distance / m.total, true
}

func splashUnitVector(dx, dy float64) (float64, float64) {
	d := splashDistance(dx, dy)
	if d == 0 {
		return 0, -1
	}
	return dx / d, dy / d
}

func splashDistance(dx, dy float64) float64 {
	return math.Sqrt(dx*dx + dy*dy)
}

func splashStemProgress(part rune, frame, start, end int) float64 {
	if part == 'L' || part == 'R' {
		return splashEaseOutCubicProgress(frame, start, end)
	}
	return splashEaseOutProgress(frame, start, end)
}

func splashEaseOutProgress(frame, start, end int) float64 {
	if end <= start || frame >= end {
		return 1
	}
	if frame <= start {
		return 0
	}
	t := float64(frame-start) / float64(end-start)
	return 1 - (1-t)*(1-t)
}

func splashEaseOutCubicProgress(frame, start, end int) float64 {
	if end <= start || frame >= end {
		return 1
	}
	if frame <= start {
		return 0
	}
	t := float64(frame-start) / float64(end-start)
	return 1 - (1-t)*(1-t)*(1-t)
}

func splashCanopyRevealedAt(row, col, frame int, geometry splashRevealGeometry) bool {
	if frame < splashCanopyStartFrame {
		return false
	}
	start := splashCanopyStartFrame
	path := geometry.canopyLeft
	if col > SplashWidth/2 {
		start += splashCanopyRightStartDelay
		path = geometry.canopyRight
	}
	if frame < start {
		return false
	}
	span := splashCanopyEndFrame - start
	if span <= 0 {
		return true
	}
	t := splashEaseOutProgress(frame, start, splashCanopyEndFrame)
	cellProgress, ok := path.cellProgress(row, col, splashCanopyRevealLead)
	return ok && cellProgress <= t
}

func splashFinalFrameLines() []string {
	out := make([]string, SplashHeight)
	for i := range out {
		if i < len(splashFinalFrame) {
			out[i] = splashFixedWidthLine(splashFinalFrame[i])
		} else {
			out[i] = splashBlankLine()
		}
	}
	return out
}

func splashFinalPartLines() []string {
	out := make([]string, SplashHeight)
	for i := range out {
		if i < len(splashFinalParts) {
			out[i] = splashFixedWidthLine(splashFinalParts[i])
		} else {
			out[i] = splashBlankLine()
		}
	}
	return out
}

func splashIsInk(r rune) bool {
	return r != ' '
}

func splashFixedWidthLine(line string) string {
	runes := []rune(line)
	if len(runes) > SplashWidth {
		return string(runes[:SplashWidth])
	}
	if len(runes) < SplashWidth {
		return line + strings.Repeat(" ", SplashWidth-len(runes))
	}
	return line
}

func splashBlankLine() string {
	return strings.Repeat(" ", SplashWidth)
}

var splashFinalFrame = []string{
	"                   ▗▄▄▟████▙▄▄▖                  ",
	"               ▄▄▟██████████████▙▄▄              ",
	"           ▄▄███████▀▀▘    ▝▀▀███████▄▄          ",
	"       ▄▄███████▀▀     ▄▄▄▄     ▀▀███████▄▄      ",
	"     ▄█████▛▀▀         ████         ▀▀▜█████▄    ",
	"   ▗▟███▛▀             ████             ▀▜███▙▖  ",
	"   ▟███▛   ▄█▙▄▖       ████       ▗▄▟█▄   ▜███▙  ",
	"   ████   ▝██████▖     ████     ▗██████▘   ████ ",
	"   ████      ▀████▌    ████    ▐████▀      ████ ",
	"   ████       ▐███▌    ████    ▐███▌       ████ ",
	"              ▐███▌    ████    ▐███▌            ",
	"              ▐███▌    ████    ▐███▌            ",
	"              ▐███▌    ████    ▐███▌            ",
	"                       ████                      ",
}

var splashFinalParts = []string{
	"                   CCCCCCCCCCCC                 ",
	"               CCCCCCCCCCCCCCCCCCCC             ",
	"           CCCCCCCCCCCC    CCCCCCCCCCCC         ",
	"       CCCCCCCCCCC     TTTT     CCCCCCCCCCC     ",
	"     CCCCCCCCC         TTTT         CCCCCCCCC   ",
	"   CCCCCCC             TTTT             CCCCCCC ",
	"   CCCCC   LLLLL       TTTT       RRRRR   CCCCC ",
	"   CCCC   LLLLLLLL     TTTT     RRRRRRRR   CCCC ",
	"   CCCC      LLLLLL    TTTT    RRRRRR      CCCC ",
	"   CCCC       LLLLL    TTTT    RRRRR       CCCC ",
	"              LLLLL    TTTT    RRRRR            ",
	"              LLLLL    TTTT    RRRRR            ",
	"              LLLLL    TTTT    RRRRR            ",
	"                       TTTT                     ",
}

// splashFrames holds the pre-rendered animation; splash.view indexes it by frame.
var splashFrames = []string{
	`                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                      ████                      `,
	`                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                      ████                      
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                                                
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
                                                
                                                
                                                
                                                
                                                
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
                                                
                                                
                                                
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
                                                
                                                
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
                      ████                      
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
                                                
                                                
                      ████                      
                      ████                      
                      ████                      
                      ████                      
          ██████      ████      ██████          
           ██████     ████     ██████           
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
                                                
                                                
                      ████                      
                      ████                      
                      ████                      
           ████       ████       ████           
          ██████      ████      ██████          
           ██████     ████     ██████           
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
                                                
                                                
                      ████                      
                      ████                      
                      ████                      
           ████       ████       ████           
  █       ██████      ████      ██████       █  
  ██       ██████     ████     ██████       ██  
  ███        ████     ████     ████        ███  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
                                                
                                                
                      ████                      
    █                 ████                 █    
   ███                ████                ███   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
                                                
                                                
      ██              ████              ██      
    █████             ████             █████    
   █████              ████              █████   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
                                                
        ███                          ███        
      ██████          ████          ██████      
    ██████            ████            ██████    
   █████              ████              █████   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
                                                
          ████                    ████          
        ███████                  ███████        
      ██████          ████          ██████      
    ██████            ████            ██████    
   █████              ████              █████   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
              ██                ██              
          ███████              ███████          
        ███████                  ███████        
      ██████          ████          ██████      
    ██████            ████            ██████    
   █████              ████              █████   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                                                
              █████          █████              
          █████████          █████████          
        ███████                  ███████        
      ██████          ████          ██████      
    ██████            ████            ██████    
   █████              ████              █████   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                  ██        ██                  
              ███████      ███████              
          █████████          █████████          
        ███████                  ███████        
      ██████          ████          ██████      
    ██████            ████            ██████    
   █████              ████              █████   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                  ███      ███                  
              ████████    ████████              
          █████████          █████████          
        ███████                  ███████        
      ██████          ████          ██████      
    ██████            ████            ██████    
   █████              ████              █████   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                  █████  █████                  
              ████████████████████              
          █████████          █████████          
        ███████                  ███████        
      ██████          ████          ██████      
    ██████            ████            ██████    
   █████              ████              █████   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                  █████  █████                  
              ████████████████████              
          █████████          █████████          
        ███████                  ███████        
      ██████          ████          ██████      
    ██████            ████            ██████    
   █████              ████              █████   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
	`                  ████████████                  
              ████████████████████              
          █████████          █████████          
        ███████                  ███████        
      ██████          ████          ██████      
    ██████            ████            ██████    
   █████              ████              █████   
  █████    ████       ████       ████    █████  
  ████    ██████      ████      ██████    ████  
  ████     ██████     ████     ██████     ████  
  ████       ████     ████     ████       ████  
   ███       ████     ████     ████       ███   
             ████     ████     ████             
             ████     ████     ████             
             ████     ████     ████             
                      ████                      
                      ████                      
                      ████                      `,
}
