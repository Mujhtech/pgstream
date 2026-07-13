package views

import "github.com/charmbracelet/lipgloss"

var DocStyle = lipgloss.
	NewStyle().
	Margin(3, 2, 1, 2).
	Padding(1, 2)

var BasicLayout = lipgloss.
	NewStyle().
	Margin(1, 0).
	PaddingLeft(2)

func GetInfoMessage(message string) string {
	return lipgloss.NewStyle().Padding(1, 1).Render(message)
}
