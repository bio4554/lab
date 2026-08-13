package main

import "github.com/charmbracelet/lipgloss"

// The palette translates the mockups' Nocturne roles onto the
// 256-color cube with adaptive fallbacks: one accent (chrome, focus,
// "you"), a second accent (agent speech), and a neutral ramp.
var (
	cAccent   = lipgloss.AdaptiveColor{Light: "56", Dark: "104"}  // violet
	cAccent2  = lipgloss.AdaptiveColor{Light: "30", Dark: "43"}   // teal — agent voice
	cGood     = lipgloss.AdaptiveColor{Light: "28", Dark: "78"}   // ✓ / done
	cBad      = lipgloss.AdaptiveColor{Light: "124", Dark: "167"} // errors
	cText     = lipgloss.AdaptiveColor{Light: "235", Dark: "252"}
	cMuted    = lipgloss.AdaptiveColor{Light: "243", Dark: "246"}
	cDim      = lipgloss.AdaptiveColor{Light: "248", Dark: "240"}
	cFaint    = lipgloss.AdaptiveColor{Light: "251", Dark: "237"}
	cHair     = lipgloss.AdaptiveColor{Light: "254", Dark: "236"} // hairlines
	cSelBG    = lipgloss.AdaptiveColor{Light: "189", Dark: "54"}  // selected row
	cBadgeBG  = lipgloss.AdaptiveColor{Light: "254", Dark: "235"}
	cAccentHi = lipgloss.AdaptiveColor{Light: "55", Dark: "147"}
)

var (
	sAccent  = lipgloss.NewStyle().Foreground(cAccent)
	sAccent2 = lipgloss.NewStyle().Foreground(cAccent2)
	sGood    = lipgloss.NewStyle().Foreground(cGood)
	sBad     = lipgloss.NewStyle().Foreground(cBad)
	sText    = lipgloss.NewStyle().Foreground(cText)
	sMuted   = lipgloss.NewStyle().Foreground(cMuted)
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sFaint   = lipgloss.NewStyle().Foreground(cFaint)
	sKey     = lipgloss.NewStyle().Foreground(cMuted).Bold(true)
	sTitle   = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sLabel   = lipgloss.NewStyle().Foreground(cDim) // section headers (PROJECTS)

	// Selected row: subtle background wash, per direction 1c.
	sSelRow = lipgloss.NewStyle().Background(cSelBG).Foreground(cAccentHi)

	// Active tab pill.
	sTabOn  = lipgloss.NewStyle().Background(cSelBG).Foreground(cAccentHi).Padding(0, 1)
	sTabOff = lipgloss.NewStyle().Foreground(cDim).Padding(0, 1)

	// Focused element: box + accent left edge (direction 1c).
	sFocusBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(cAccent).
			BorderLeft(true).BorderLeftForeground(cAccent)
	sBlurBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(cHair)

	// Modal dialog over the dimmed frame (2a/2b).
	sModal = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cAccent).
		Padding(0, 1)
)

// hairline draws a horizontal rule of the given width.
func hairline(width int) string {
	if width <= 0 {
		return ""
	}
	line := make([]rune, width)
	for i := range line {
		line[i] = '─'
	}
	return sFaint.Render(string(line))
}
