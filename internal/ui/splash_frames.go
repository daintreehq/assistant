package ui

import "strings"

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
	partBounds := splashPartBounds(finalParts)
	out := make([]string, SplashHeight)
	for y := range out {
		dst := []rune(splashBlankLine())
		finalRunes := []rune(finalFrame[y])
		partRunes := []rune(finalParts[y])
		for x := 0; x < SplashWidth; x++ {
			if splashPartRevealedAt(y, x, partRunes[x], idx, partBounds) && splashIsInk(finalRunes[x]) {
				dst[x] = finalRunes[x]
			}
		}
		out[y] = string(dst)
	}
	return out
}

func splashPartRevealedAt(row, col int, part rune, frame int, partBounds map[rune]splashVerticalBounds) bool {
	switch part {
	case 'C':
		return splashCanopyRevealedAt(row, col, frame)
	case 'L':
		return splashStemRevealedAt('L', row, frame, splashLeftBranchStartFrame, splashLeftBranchEndFrame, partBounds)
	case 'R':
		return splashStemRevealedAt('R', row, frame, splashRightBranchStartFrame, splashRightBranchEndFrame, partBounds)
	case 'T':
		return splashStemRevealedAt('T', row, frame, splashTrunkStartFrame, splashTrunkEndFrame, partBounds)
	default:
		return false
	}
}

func splashStemRevealedAt(part rune, row, frame, start, end int, partBounds map[rune]splashVerticalBounds) bool {
	if frame < start {
		return false
	}
	if frame >= end {
		return true
	}
	bounds, ok := partBounds[part]
	if !ok || row < bounds.minRow || row > bounds.maxRow {
		return false
	}
	progress := splashSmoothProgress(frame, start, end)
	rowProgress := float64(bounds.maxRow-row) / float64(bounds.maxRow-bounds.minRow)
	return rowProgress <= progress
}

type splashVerticalBounds struct {
	minRow int
	maxRow int
}

func splashPartBounds(lines []string) map[rune]splashVerticalBounds {
	bounds := make(map[rune]splashVerticalBounds)
	for y, line := range lines {
		for _, r := range line {
			if r == ' ' {
				continue
			}
			b, ok := bounds[r]
			if !ok {
				bounds[r] = splashVerticalBounds{minRow: y, maxRow: y}
				continue
			}
			if y < b.minRow {
				b.minRow = y
			}
			if y > b.maxRow {
				b.maxRow = y
			}
			bounds[r] = b
		}
	}
	return bounds
}

func splashSmoothProgress(frame, start, end int) float64 {
	if end <= start || frame >= end {
		return 1
	}
	if frame <= start {
		return 0
	}
	t := float64(frame-start) / float64(end-start)
	return t * t * (3 - 2*t)
}

func splashCanopyRevealedAt(row, col, frame int) bool {
	if frame < splashCanopyStartFrame {
		return false
	}
	start := splashCanopyStartFrame
	if col >= SplashWidth/2 {
		start += splashCanopyRightStartDelay
	}
	if frame < start {
		return false
	}
	span := splashCanopyEndFrame - start
	if span <= 0 {
		return true
	}
	t := float64(frame-start) / float64(span)
	if t > 1 {
		t = 1
	}
	return splashCanopyCellProgress(row, col) <= t
}

func splashCanopyCellProgress(row, col int) float64 {
	x := col
	if col >= SplashWidth/2 {
		x = SplashWidth - 1 - col
	}
	const (
		startX = 3.0
		endX   = 23.0
		startY = 9.0
	)
	alongX := (float64(x) - startX) / (endX - startX)
	if alongX < 0 {
		alongX = 0
	} else if alongX > 1 {
		alongX = 1
	}
	upY := (startY - float64(row)) / startY
	if upY < 0 {
		upY = 0
	} else if upY > 1 {
		upY = 1
	}
	return 0.68*alongX + 0.32*upY
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
