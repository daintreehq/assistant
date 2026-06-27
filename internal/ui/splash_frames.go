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
// The rendered pixels now come from the original Daintree SVG geometry in
// splash_vector.go: the vector fill paths are intersected with the animated SVG mask
// strokes, supersampled onto a terminal grid, then encoded as Unicode block glyphs.
// The old source masks below are retained as a coarse historical reference and shape
// guard for tests.

func splashFrameLines(idx int) []string {
	rows := splashFrameRows(idx)
	out := make([]string, SplashHeight)
	for i, row := range rows {
		out[i] = splashCellsPlain(row)
	}
	return out
}

func splashFrameRows(idx int) [][]splashCell {
	return splashVectorFrameRows(idx)
}

func splashCellsPlain(cells []splashCell) string {
	var b strings.Builder
	for _, cell := range cells {
		if cell.glyph == 0 {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(cell.glyph)
	}
	return b.String()
}

func splashDistance(dx, dy float64) float64 {
	return math.Sqrt(dx*dx + dy*dy)
}

func splashFinalFrameLines() []string {
	return splashFrameLines(splashCanopyEndFrame)
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
