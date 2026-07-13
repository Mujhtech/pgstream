package views

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/gamut"
)

var (
	LightGray = lipgloss.AdaptiveColor{Light: "#828282", Dark: "#828282"}
	Primary   = lipgloss.AdaptiveColor{Light: "#F25D94", Dark: "#F25D94"}
	Green     = lipgloss.AdaptiveColor{Light: "#23cc71", Dark: "#23cc71"}
	White     = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#ffffff"}
	Subtle    = lipgloss.AdaptiveColor{Light: "#D9DCCF", Dark: "#383838"}
	Blends    = gamut.Blends(lipgloss.Color("#F25D94"), lipgloss.Color("#EDFF82"), 50)
)
