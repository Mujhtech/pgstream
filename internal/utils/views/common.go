package views

import "github.com/charmbracelet/lipgloss"

var DocStyle = lipgloss.
	NewStyle().
	Margin(3, 2, 1, 2).
	Padding(1, 2)

// GetInfoMessage renders a transient single-line status (used by the inline
// spinner); no vertical padding so the surrounding log stays compact.
func GetInfoMessage(message string) string {
	return lipgloss.NewStyle().Padding(0, 1).Render(message)
}
