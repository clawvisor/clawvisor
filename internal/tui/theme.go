package tui

import "github.com/charmbracelet/lipgloss"

// Colors matching the web dashboard.
var (
	ColorSurface      = lipgloss.Color("#0f1117")
	ColorSurfaceLight = lipgloss.Color("#161923")
	ColorBrand        = lipgloss.Color("#818cf8") // indigo
	ColorAccent       = lipgloss.Color("#f59e0b") // amber
	ColorGreen        = lipgloss.Color("#22c55e")
	ColorRed          = lipgloss.Color("#ef4444")
	ColorOrange       = lipgloss.Color("#f97316")
	ColorDim          = lipgloss.Color("#6b7280")
	ColorWhite        = lipgloss.Color("#e5e7eb")
	ColorYellow       = lipgloss.Color("#eab308")
)

// Shared styles.
var (
	StyleBrand = lipgloss.NewStyle().Foreground(ColorBrand).Bold(true)
	StyleDim   = lipgloss.NewStyle().Foreground(ColorDim)
	StyleGreen = lipgloss.NewStyle().Foreground(ColorGreen)
	StyleRed   = lipgloss.NewStyle().Foreground(ColorRed)
	StyleAmber  = lipgloss.NewStyle().Foreground(ColorAccent)
	StyleOrange = lipgloss.NewStyle().Foreground(ColorOrange)
	StyleBold   = lipgloss.NewStyle().Bold(true)
	StyleWhite = lipgloss.NewStyle().Foreground(ColorWhite)

	// Sidebar
	StyleSidebarActive   = lipgloss.NewStyle().Foreground(ColorBrand).Bold(true)
	StyleSidebarInactive = lipgloss.NewStyle().Foreground(ColorDim)
	StyleSidebarBadge    = lipgloss.NewStyle().Foreground(ColorAccent).Bold(true)

	// Status bar
	StyleStatusBar = lipgloss.NewStyle().
			Foreground(ColorDim).
			Background(ColorSurfaceLight).
			Padding(0, 1)

	StyleStatusKey = lipgloss.NewStyle().
			Foreground(ColorBrand).
			Background(ColorSurfaceLight).
			Bold(true)

	// Table header
	StyleTableHeader = lipgloss.NewStyle().
				Foreground(ColorDim).
				Bold(true)

	// Detail overlay border
	StyleOverlayBorder = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(ColorBrand).
				Padding(1, 2)
)
