package ui

import "strings"

// The splash animation frames, maintained directly as Go.

// SPLASH_FRAMES: 20 source frames, each 18 lines of 48 columns using U+2588 (█)
// blocks and spaces. The mark draws
// itself in: trunk grows up (0-3), roots (4-6), then the canopy arch on 45° diagonals
// up to the centre peak that connects last (7-19); frame 19 is the complete logo.
//
// Rendering uses these source frames only as a reveal mask. The visible pixels come
// from splashFinalFrame, a 14-row terminal raster of the real Daintree SVG using
// Unicode quadrant/half/full block glyphs. That keeps the historical 18-row boot
// envelope while compensating for xterm/WebGL's tall character cells and preserving
// the logo's curved silhouette.

func splashFrameLines(idx int) []string {
	finalFrame := splashFinalFrameLines()
	finalParts := splashFinalPartLines()
	reveal := splashRevealMaskLines(splashSourceFrameFor(idx))
	out := make([]string, SplashHeight)
	for y := range out {
		dst := []rune(splashBlankLine())
		finalRunes := []rune(finalFrame[y])
		partRunes := []rune(finalParts[y])
		for x := 0; x < SplashWidth; x++ {
			if splashPartRevealedAt(reveal, partRunes, y, x, partRunes[x], idx) && splashIsInk(finalRunes[x]) {
				dst[x] = finalRunes[x]
			}
		}
		out[y] = string(dst)
	}
	return out
}

func splashSourceFrameFor(frame int) int {
	if frame < 0 {
		return 0
	}
	if frame >= splashCanopyEndFrame {
		return splashSourceFrames - 1
	}
	return (frame*(splashSourceFrames-1) + splashCanopyEndFrame/2) / splashCanopyEndFrame
}

func splashPartRevealedAt(reveal []string, partRunes []rune, row, col int, part rune, frame int) bool {
	if part == ' ' {
		return false
	}
	if part == 'C' {
		return splashCanopyRevealedAt(row, col, frame)
	}
	start, end := splashPartRun(partRunes, col, part)
	for x := start; x < end; x++ {
		if splashMaskRevealedAt(reveal, row, x) {
			return true
		}
	}
	return false
}

func splashPartRun(partRunes []rune, col int, part rune) (int, int) {
	start := col
	for start > 0 && partRunes[start-1] == part {
		start--
	}
	end := col + 1
	for end < len(partRunes) && partRunes[end] == part {
		end++
	}
	return start, end
}

func splashMaskRevealedAt(reveal []string, row, col int) bool {
	for y := row - 1; y <= row+1; y++ {
		if y < 0 || y >= len(reveal) {
			continue
		}
		line := []rune(splashFixedWidthLine(reveal[y]))
		for x := col - 2; x <= col+2; x++ {
			if x >= 0 && x < len(line) && splashIsInk(line[x]) {
				return true
			}
		}
	}
	return false
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

func splashRevealMaskLines(idx int) []string {
	if idx < 0 {
		idx = 0
	}
	if idx >= len(splashFrames) {
		idx = len(splashFrames) - 1
	}
	lines := strings.Split(splashFrames[idx], "\n")
	if len(lines) < SplashHeight {
		for len(lines) < SplashHeight {
			lines = append(lines, splashBlankLine())
		}
	} else if len(lines) > SplashHeight {
		lines = lines[:SplashHeight]
	}
	out := make([]string, SplashHeight)
	for i := range out {
		out[i] = splashBlankLine()
	}
	for srcRow, line := range lines {
		dstRow := splashResampledRow(srcRow)
		out[dstRow] = overlaySplashLine(out[dstRow], line)
	}
	return out
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

func splashResampledRow(srcRow int) int {
	if splashVisibleHeight <= 1 {
		return 0
	}
	if srcRow < 0 {
		srcRow = 0
	}
	if srcRow >= SplashHeight {
		srcRow = SplashHeight - 1
	}
	return (srcRow*(splashVisibleHeight-1) + (SplashHeight-1)/2) / (SplashHeight - 1)
}

func splashIsInk(r rune) bool {
	return r != ' '
}

func overlaySplashLine(base, extra string) string {
	dst := []rune(splashFixedWidthLine(base))
	for i, r := range []rune(splashFixedWidthLine(extra)) {
		if r != ' ' {
			dst[i] = r
		}
	}
	return string(dst)
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
	"   ████▌  ▝██████▖     ████     ▗██████▘  ▐████  ",
	"   ████▌     ▀████▌    ████    ▐████▀     ▐████  ",
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
	"   CCCCC  LLLLLLLL     TTTT     RRRRRRRR  CCCCC ",
	"   CCCCC     LLLLLL    TTTT    RRRRRR     CCCCC ",
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
