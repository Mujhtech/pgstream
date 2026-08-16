package session

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mujhtech/pgstream/internal/migrator"
	"github.com/mujhtech/pgstream/internal/utils/views"
)

const (
	// MySQL fields
	mysqlHost = iota
	mysqlPort
	mysqlUser
	mysqlPassword
	mysqlDatabase

	// PostgreSQL fields
	postgresHost
	postgresPort
	postgresUser
	postgresPassword
	postgresDatabase
	postgresSchema

	fieldCount
	// buttonIndex is the focus position of the Continue button, one past the
	// last input.
	buttonIndex = fieldCount
)

const darkGray = lipgloss.Color("#767676")

var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	sectionStyle  = lipgloss.NewStyle().Foreground(views.Primary).Bold(true)
	labelFocused  = lipgloss.NewStyle().Foreground(views.Primary).Bold(true)
	labelBlurred  = lipgloss.NewStyle().Foreground(darkGray)
	markerFocused = lipgloss.NewStyle().Foreground(views.Primary).Render("▸ ")
	markerBlurred = "  "
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	helpStyle     = lipgloss.NewStyle().Foreground(darkGray)
	buttonFocused = lipgloss.NewStyle().Bold(true).Foreground(views.White).Background(views.Primary).Padding(0, 2)
	buttonBlurred = lipgloss.NewStyle().Foreground(darkGray).Padding(0, 2)

	ErrCancelled = errors.New("session setup cancelled")
)

// fieldSpec declares one form input; initialModel builds the whole form from
// the table below so layout, defaults, and validation live in one place.
type fieldSpec struct {
	label       string
	placeholder string
	initial     string
	width       int
	charLimit   int
	password    bool
	validate    func(string) error
}

var fieldSpecs = [fieldCount]fieldSpec{
	mysqlHost:        {label: "Host", placeholder: "127.0.0.1", initial: "127.0.0.1", width: 30, charLimit: 255, validate: hostValidator},
	mysqlPort:        {label: "Port", placeholder: "3306", initial: "3306", width: 6, charLimit: 5, validate: portValidator},
	mysqlUser:        {label: "User", placeholder: "root", initial: "root", width: 24, charLimit: 255, validate: userValidator},
	mysqlPassword:    {label: "Password", placeholder: "password", width: 24, charLimit: 255, password: true, validate: passwordValidator},
	mysqlDatabase:    {label: "Database", placeholder: "mydb", width: 24, charLimit: 64, validate: databaseValidator},
	postgresHost:     {label: "Host", placeholder: "127.0.0.1", initial: "127.0.0.1", width: 30, charLimit: 255, validate: hostValidator},
	postgresPort:     {label: "Port", placeholder: "5432", initial: "5432", width: 6, charLimit: 5, validate: portValidator},
	postgresUser:     {label: "User", placeholder: "postgres", initial: "postgres", width: 24, charLimit: 255, validate: userValidator},
	postgresPassword: {label: "Password", placeholder: "password", width: 24, charLimit: 255, password: true, validate: passwordValidator},
	postgresDatabase: {label: "Database", placeholder: "mydb", width: 24, charLimit: 64, validate: databaseValidator},
	postgresSchema:   {label: "Schema", placeholder: "public", initial: "public", width: 24, charLimit: 64, validate: schemaValidator},
}

var formSections = []struct {
	title string
	from  int
	to    int
}{
	{title: "MySQL", from: mysqlHost, to: mysqlDatabase},
	{title: "PostgreSQL", from: postgresHost, to: postgresSchema},
}

type model struct {
	inputs    []textinput.Model
	focused   int
	err       error
	submitted bool
	cancelled bool
}

// Validator functions to ensure valid input
func hostValidator(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("host cannot be empty")
	}
	return nil
}

func portValidator(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("port cannot be empty")
	}
	port, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return fmt.Errorf("port must be a number")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func userValidator(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("user cannot be empty")
	}
	return nil
}

func passwordValidator(s string) error {
	// Password can be empty for some configurations
	return nil
}

func databaseValidator(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("database cannot be empty")
	}
	return nil
}

func schemaValidator(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("schema cannot be empty")
	}
	return nil
}

func initialModel() model {
	inputs := make([]textinput.Model, fieldCount)
	for index, spec := range fieldSpecs {
		input := textinput.New()
		input.Placeholder = spec.placeholder
		input.CharLimit = spec.charLimit
		input.Width = spec.width
		input.Prompt = ""
		input.Validate = spec.validate
		if spec.password {
			input.EchoMode = textinput.EchoPassword
		}
		if spec.initial != "" {
			input.SetValue(spec.initial)
		}
		inputs[index] = input
	}
	inputs[mysqlHost].Focus()

	return model{
		inputs:  inputs,
		focused: mysqlHost,
	}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, len(m.inputs))

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			if m.focused == buttonIndex {
				if err := m.validateAllInputs(); err != nil {
					m.syncFocus()
					return m, nil
				}
				m.submitted = true
				return m, tea.Quit
			}
			if err := m.validateInput(m.focused); err != nil {
				return m, nil
			}
			m.nextInput()
		case tea.KeyCtrlC, tea.KeyEsc:
			m.cancelled = true
			return m, tea.Quit
		case tea.KeyShiftTab, tea.KeyCtrlP, tea.KeyUp:
			// Moving backwards never blocks on validation.
			m.prevInput()
		case tea.KeyTab, tea.KeyCtrlN, tea.KeyDown:
			if m.focused != buttonIndex {
				if err := m.validateInput(m.focused); err != nil {
					return m, nil
				}
			}
			m.nextInput()
		}
		m.syncFocus()
	}

	for i := range m.inputs {
		m.inputs[i], cmds[i] = m.inputs[i].Update(msg)
	}
	if m.focused < fieldCount {
		m.err = m.inputs[m.focused].Err
	}
	return m, tea.Batch(cmds...)
}

// syncFocus blurs every input and focuses the current one (none when the
// Continue button holds focus).
func (m *model) syncFocus() {
	for i := range m.inputs {
		m.inputs[i].Blur()
	}
	if m.focused < fieldCount {
		m.inputs[m.focused].Focus()
	}
}

func (m model) View() string {
	var view strings.Builder
	view.WriteString("\n  ")
	view.WriteString(titleStyle.Render("pgstream — start a new migration session"))
	view.WriteString("\n")

	for _, section := range formSections {
		view.WriteString("\n  ")
		view.WriteString(sectionStyle.Render(section.title))
		view.WriteString("\n")
		for index := section.from; index <= section.to; index++ {
			marker, label := markerBlurred, labelBlurred
			if index == m.focused {
				marker, label = markerFocused, labelFocused
			}
			fmt.Fprintf(&view, "  %s%s %s\n", marker, label.Width(10).Render(fieldSpecs[index].label), m.inputs[index].View())
		}
	}

	view.WriteString("\n  ")
	if m.focused == buttonIndex {
		view.WriteString(buttonFocused.Render("Continue"))
	} else {
		view.WriteString(buttonBlurred.Render("[ Continue ]"))
	}
	view.WriteString("\n")

	if m.err != nil {
		view.WriteString("\n  ")
		view.WriteString(errorStyle.Render("✗ " + m.err.Error()))
		view.WriteString("\n")
	}

	view.WriteString("\n  ")
	view.WriteString(helpStyle.Render("tab/↓ next · shift+tab/↑ back · enter continue · esc cancel"))
	view.WriteString("\n")
	return view.String()
}

func (m *model) validateInput(index int) error {
	if index < 0 || index >= len(m.inputs) {
		return fmt.Errorf("invalid input index %d", index)
	}
	validator := m.inputs[index].Validate
	if validator == nil {
		m.inputs[index].Err = nil
		m.err = nil
		return nil
	}
	err := validator(m.inputs[index].Value())
	m.inputs[index].Err = err
	m.err = err
	return err
}

func (m *model) validateAllInputs() error {
	for index := range m.inputs {
		if err := m.validateInput(index); err != nil {
			m.focused = index
			return err
		}
	}
	return nil
}

// nextInput focuses the next input field, including the Continue button.
func (m *model) nextInput() {
	m.focused = (m.focused + 1) % (fieldCount + 1)
}

// prevInput focuses the previous input field, wrapping through the button.
func (m *model) prevInput() {
	m.focused--
	if m.focused < 0 {
		m.focused = buttonIndex
	}
}

func Run() (migrator.Config, error) {
	m := initialModel()

	finalModel, err := tea.NewProgram(m).Run()
	if err != nil {
		return migrator.Config{}, err
	}
	result, ok := finalModel.(model)
	if !ok {
		return migrator.Config{}, fmt.Errorf("unexpected terminal model type %T", finalModel)
	}
	if result.cancelled {
		return migrator.Config{}, ErrCancelled
	}
	if !result.submitted {
		return migrator.Config{}, fmt.Errorf("session setup ended without submission")
	}

	config := migrator.Config{
		MySQL: &migrator.MySQLConfig{
			Host:     result.inputs[mysqlHost].Value(),
			Port:     result.inputs[mysqlPort].Value(),
			User:     result.inputs[mysqlUser].Value(),
			Password: result.inputs[mysqlPassword].Value(),
			Database: result.inputs[mysqlDatabase].Value(),
		},
		PostgreSQL: &migrator.PostgreSQLConfig{
			Host:     result.inputs[postgresHost].Value(),
			Port:     result.inputs[postgresPort].Value(),
			User:     result.inputs[postgresUser].Value(),
			Password: result.inputs[postgresPassword].Value(),
			Database: result.inputs[postgresDatabase].Value(),
			Schema:   result.inputs[postgresSchema].Value(),
		},
	}

	return config, nil
}
