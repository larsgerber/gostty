package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

//go:embed animation-data.json
var animationJSON []byte

// Frame represents a single animation frame
type Frame struct {
	Lines []string
}

// Animation handles the animation logic
type Animation struct {
	frames         []Frame
	highlightColor string
	frameCount     int
}

// Constants
const (
	ImageWidth      = 77
	ImageHeight     = 41
	MicrosPerFrame  = 35000
	FrameDelay      = MicrosPerFrame / 1000
	ClearScreen     = "\x1b[2J"
	EraseLineRight  = "\x1b[K"
	BeginSync       = "\x1b[?2026h" // Synchronized output: the terminal holds rendering...
	EndSync         = "\x1b[?2026l" // ...until the frame is complete (ignored if unsupported)
	DefaultDuration = 0
	ColorStartTag   = "<c>"
	ColorEndTag     = "</c>"
	ResetColor      = "\x1b[0m"
)

// Color mapping
var colorMap = map[string]string{
	"black":         "\x1b[30m",
	"red":           "\x1b[31m",
	"green":         "\x1b[32m",
	"yellow":        "\x1b[33m",
	"blue":          "\x1b[34m",
	"magenta":       "\x1b[35m",
	"cyan":          "\x1b[36m",
	"white":         "\x1b[37m",
	"brightblack":   "\x1b[90m",
	"brightred":     "\x1b[91m",
	"brightgreen":   "\x1b[92m",
	"brightyellow":  "\x1b[93m",
	"brightblue":    "\x1b[94m",
	"brightmagenta": "\x1b[95m",
	"brightcyan":    "\x1b[96m",
	"brightwhite":   "\x1b[97m",
}

// NewAnimation creates a new animation instance
func NewAnimation() *Animation {
	return &Animation{
		highlightColor: "\x1b[34m", // Default blue
	}
}

// SetHighlightColor sets the highlight color for the animation
func (a *Animation) SetHighlightColor(color string) {
	a.highlightColor = color
}

// Initialize processes animation data and pre-calculates frames
func (a *Animation) Initialize(animationData [][]string) {
	a.frames = make([]Frame, len(animationData))
	a.frameCount = len(animationData)

	for frameIndex, frameLines := range animationData {
		processedLines := make([]string, len(frameLines))

		for lineIndex, line := range frameLines {
			// Trailing spaces are dropped: the renderer erases the rest of each line
			// anyway, and some blank lines are wider than the image
			processedLines[lineIndex] = a.processColorCodes(strings.TrimRight(line, " "))
		}

		a.frames[frameIndex] = Frame{Lines: processedLines}
	}
}

// loadAnimationData loads animation data from a JSON file
func loadAnimationDataFromEmbedded() ([][]string, error) {
	var animationData [][]string
	err := json.Unmarshal(animationJSON, &animationData)
	if err != nil {
		return nil, err
	}
	return animationData, nil
}

// processColorCodes processes color tags in a line
func (a *Animation) processColorCodes(line string) string {
	var result strings.Builder
	currentIndex := 0

	for {
		colorStart := strings.Index(line[currentIndex:], ColorStartTag)
		if colorStart == -1 {
			// No more color tags, add remaining content
			if currentIndex < len(line) {
				result.WriteString(line[currentIndex:])
			}
			break
		}

		colorStart += currentIndex

		// Add content before color tag
		if colorStart > currentIndex {
			result.WriteString(line[currentIndex:colorStart])
		}

		// Find the end of colored section
		contentStart := colorStart + len(ColorStartTag)
		colorEnd := strings.Index(line[contentStart:], ColorEndTag)
		if colorEnd == -1 {
			// Malformed tag, just add the rest
			result.WriteString(line[currentIndex:])
			break
		}

		colorEnd += contentStart

		// Add colored content
		result.WriteString(a.highlightColor)
		result.WriteString(line[contentStart:colorEnd])
		result.WriteString(ResetColor)

		currentIndex = colorEnd + len(ColorEndTag)
	}

	return result.String()
}

// GetFrameLines returns the lines for a specific frame
func (a *Animation) GetFrameLines(index int) []string {
	if index < 0 || index >= len(a.frames) {
		return nil
	}
	return a.frames[index].Lines
}

// FrameCount returns the total number of frames
func (a *Animation) FrameCount() int {
	return a.frameCount
}

// Terminal represents terminal state and operations
type Terminal struct {
	width          int
	height         int
	needsClear     bool
	lastFrameIndex int
	outputBuffer   []byte
}

// NewTerminal creates a new terminal instance
func NewTerminal() *Terminal {
	width, height := terminalSize()

	return &Terminal{
		width:          width,
		height:         height,
		needsClear:     true,
		lastFrameIndex: -1,
		outputBuffer:   make([]byte, 0, 64*1024), // 64KB buffer
	}
}

// terminalSize returns the terminal dimensions, falling back to 80x24 when
// they cannot be determined (e.g. stdout is not a terminal)
func terminalSize() (int, int) {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 || height <= 0 {
		return 80, 24
	}
	return width, height
}

// WriteToBuffer writes string to output buffer
func (t *Terminal) WriteToBuffer(str string) {
	t.outputBuffer = append(t.outputBuffer, str...)
}

// FlushBuffer flushes the output buffer to stdout
func (t *Terminal) FlushBuffer() {
	if len(t.outputBuffer) > 0 {
		os.Stdout.Write(t.outputBuffer)
		t.outputBuffer = t.outputBuffer[:0] // Reset buffer
	}
}

// UpdateSize updates terminal dimensions
func (t *Terminal) UpdateSize() {
	width, height := terminalSize()
	if width != t.width || height != t.height {
		t.width = width
		t.height = height
		t.needsClear = true
	}
}

// RenderFrame renders a single frame.
//
// The previous frame is overwritten in place rather than clearing the screen
// first, and the whole frame is sent as one synchronized update. Otherwise
// terminals that render independently of the output stream (e.g. Ghostty) can
// show the cleared screen for a moment, which looks like flickering.
func (t *Terminal) RenderFrame(animation *Animation, frameIndex int) {
	// If nothing changed, skip rendering
	if !t.needsClear && frameIndex == t.lastFrameIndex {
		return
	}

	verticalPadding := max(0, (t.height-ImageHeight)/2)
	horizontalPadding := max(0, (t.width-ImageWidth)/2)

	// Start fresh buffer
	t.outputBuffer = t.outputBuffer[:0]
	t.WriteToBuffer(BeginSync)

	// A full clear is only needed when the layout changed
	if t.needsClear {
		t.WriteToBuffer(ClearScreen)
	}

	for i, line := range animation.GetFrameLines(frameIndex) {
		row := verticalPadding + i + 1
		if row > t.height {
			break // Terminal is shorter than the image; don't scroll
		}
		// Move to the start of the line, draw it and erase leftovers of the previous frame
		t.outputBuffer = fmt.Appendf(t.outputBuffer, "\x1b[%d;%dH", row, horizontalPadding+1)
		t.WriteToBuffer(line)
		t.WriteToBuffer(EraseLineRight)
	}

	t.WriteToBuffer(EndSync)

	// Flush the buffer to stdout
	t.FlushBuffer()
	t.needsClear = false
	t.lastFrameIndex = frameIndex
}

// Config holds application configuration
type Config struct {
	colorArg          string
	durationInSeconds int
}

// ParseArgs parses command line arguments
func ParseArgs() (*Config, error) {
	config := &Config{
		colorArg:          "\x1b[34m", // Default blue
		durationInSeconds: DefaultDuration,
	}

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--colors", "-h", "--help":
			showColorHelp()
			os.Exit(0)
		case "--color", "-c":
			if i+1 < len(args) {
				color := args[i+1]
				var digitRegex = regexp.MustCompile(`^\d+$`)
				if strings.HasPrefix(color, "\x1b[") {
					config.colorArg = color
				} else if digitRegex.MatchString(color) {
					config.colorArg = fmt.Sprintf("\x1b[%sm", color)
				} else if ansiColor, exists := colorMap[strings.ToLower(color)]; exists {
					config.colorArg = ansiColor
				} else {
					config.colorArg = "\x1b[34m" // Default to blue
				}
				i++ // Skip next argument
			}
		case "--timer", "-t":
			if i+1 < len(args) {
				if duration, err := strconv.Atoi(args[i+1]); err == nil {
					config.durationInSeconds = duration
				}
				i++ // Skip next argument
			}
		}
	}

	return config, nil
}

// showColorHelp displays available colors
func showColorHelp() {
	fmt.Println("\nAvailable colors:")
	for name, code := range colorMap {
		fmt.Printf("  %s%s%s\n", code, name, ResetColor)
	}
	fmt.Println("\nUsage:")
	fmt.Println("  gostty -c <color>        Use a color name from the list above")
	fmt.Println("  gostty -c <number>       Use an ANSI color code (30-37 or 90-97)")
	fmt.Println("  gostty --colors          Show this color help")
	fmt.Println("  gostty -t <seconds>      Run animation for specified duration")
}

// cleanup performs cleanup operations
func cleanup() {
	// End any unfinished synchronized update, re-enable line wrapping,
	// show cursor and restore main screen buffer
	fmt.Print(EndSync + "\x1b[?7h\x1b[?25h\x1b[?1049l")
}

// runAnimation runs the main animation loop
func runAnimation(animation *Animation, terminal *Terminal, config *Config) {
	start := time.Now()

	ticker := time.NewTicker(time.Millisecond * FrameDelay)
	defer ticker.Stop()

	for {
		now := time.Now()

		// Check if duration has elapsed
		elapsed := now.Sub(start)
		if config.durationInSeconds > 0 && elapsed >= time.Duration(config.durationInSeconds)*time.Second {
			return
		}

		// Update terminal size
		terminal.UpdateSize()

		// Calculate frame index based on actual animation time
		effectiveElapsed := now.Sub(start)
		frameIndex := int(effectiveElapsed.Milliseconds()/FrameDelay) % animation.FrameCount()

		terminal.RenderFrame(animation, frameIndex)

		<-ticker.C
	}
}

func main() {
	// Parse command line arguments
	config, err := ParseArgs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing arguments: %v\n", err)
		os.Exit(1)
	}

	// Load animation data from embedded JSON
	animationData, err := loadAnimationDataFromEmbedded()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load animation data: %v\n", err)
		os.Exit(1)
	}

	// Initialize animation and terminal
	animation := NewAnimation()
	terminal := NewTerminal()

	// Set highlight color based on user input
	animation.SetHighlightColor(config.colorArg)

	// Initialize animation with loaded data
	animation.Initialize(animationData)

	// Setup signal handling for cleanup
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		cleanup()
		os.Exit(0)
	}()

	// Switch to alternate screen buffer, hide cursor and disable line wrapping
	// (lines wider than the terminal get cut off instead of wrapping/scrolling)
	fmt.Print("\x1b[?1049h\x1b[?25l\x1b[?7l")

	// Cleanup on exit
	defer cleanup()

	// Start the animation
	runAnimation(animation, terminal, config)
}
