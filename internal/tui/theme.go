package tui

import "charm.land/lipgloss/v2"

// Colors follow the Hermes TUI dark theme as it runs: the default skin
// (hermes/hermes_cli skins, written to $HERMES_HOME/tui-theme-boot.json)
// over hermes/ui-tui/src/theme.ts.
var (
	colorText           = lipgloss.Color("#FFF8DC")
	colorLabel          = lipgloss.Color("#DAA520")
	colorAccent         = lipgloss.Color("#FFBF00")
	colorPrimary        = lipgloss.Color("#FFD700")
	colorBorder         = lipgloss.Color("#CD7F32")
	colorMuted          = lipgloss.Color("#B8860B")
	colorStatus         = lipgloss.Color("#C0C0C0")
	colorError          = lipgloss.Color("#EF5350")
	colorOK             = lipgloss.Color("#4CAF50")
	colorWarn           = lipgloss.Color("#FFA726")
	colorSelect         = lipgloss.Color("#333355") // the selected row of a list
	colorSessionID      = lipgloss.Color("#8B8682")
	colorStatusGood     = lipgloss.Color("#8FBC8F") // context bar: under half full
	colorStatusWarn     = lipgloss.Color("#FFD700")
	colorStatusBad      = lipgloss.Color("#FF8C00")
	colorStatusCritical = lipgloss.Color("#FF6B6B")
)

var (
	styleText    = lipgloss.NewStyle().Foreground(colorText)
	styleLabel   = lipgloss.NewStyle().Foreground(colorLabel)
	styleAccent  = lipgloss.NewStyle().Foreground(colorAccent)
	stylePrimary = lipgloss.NewStyle().Foreground(colorPrimary)
	styleBorder  = lipgloss.NewStyle().Foreground(colorBorder)
	styleMuted   = lipgloss.NewStyle().Foreground(colorMuted)
	styleDim     = lipgloss.NewStyle().Foreground(colorMuted).Faint(true)
	styleStatus  = lipgloss.NewStyle().Foreground(colorStatus)
	styleError   = lipgloss.NewStyle().Foreground(colorError)
	styleOK      = lipgloss.NewStyle().Foreground(colorOK)
	styleWarn    = lipgloss.NewStyle().Foreground(colorWarn)
)
