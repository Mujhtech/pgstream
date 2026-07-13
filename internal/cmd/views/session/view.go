package session

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mujhtech/pgstream/internal/migrator"
)

type (
	errMsg error
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
)

const (
	hotPink  = lipgloss.Color("#FF06B7")
	darkGray = lipgloss.Color("#767676")
)

var (
	inputStyle    = lipgloss.NewStyle().Foreground(hotPink)
	continueStyle = lipgloss.NewStyle().Foreground(darkGray)
	ErrCancelled  = errors.New("session setup cancelled")
)

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
	var inputs []textinput.Model = make([]textinput.Model, 11)

	// MySQL inputs
	inputs[mysqlHost] = textinput.New()
	inputs[mysqlHost].Placeholder = "127.0.0.1"
	inputs[mysqlHost].Focus()
	inputs[mysqlHost].CharLimit = 255
	inputs[mysqlHost].Width = 255
	inputs[mysqlHost].Prompt = ""
	inputs[mysqlHost].Validate = hostValidator
	inputs[mysqlHost].SetValue("127.0.0.1")

	inputs[mysqlPort] = textinput.New()
	inputs[mysqlPort].Placeholder = "3306"
	inputs[mysqlPort].CharLimit = 10
	inputs[mysqlPort].Width = 15
	inputs[mysqlPort].Prompt = ""
	inputs[mysqlPort].Validate = portValidator
	inputs[mysqlPort].SetValue("3306")

	inputs[mysqlUser] = textinput.New()
	inputs[mysqlUser].Placeholder = "root"
	inputs[mysqlUser].CharLimit = 255
	inputs[mysqlUser].Width = 30
	inputs[mysqlUser].Prompt = ""
	inputs[mysqlUser].Validate = userValidator
	inputs[mysqlUser].SetValue("root")

	inputs[mysqlPassword] = textinput.New()
	inputs[mysqlPassword].Placeholder = "password"
	inputs[mysqlPassword].CharLimit = 255
	inputs[mysqlPassword].Width = 30
	inputs[mysqlPassword].Prompt = ""
	inputs[mysqlPassword].EchoMode = textinput.EchoPassword
	inputs[mysqlPassword].Validate = passwordValidator

	inputs[mysqlDatabase] = textinput.New()
	inputs[mysqlDatabase].Placeholder = "mydb"
	inputs[mysqlDatabase].CharLimit = 50
	inputs[mysqlDatabase].Width = 30
	inputs[mysqlDatabase].Prompt = ""
	inputs[mysqlDatabase].Validate = databaseValidator

	// PostgreSQL inputs
	inputs[postgresHost] = textinput.New()
	inputs[postgresHost].Placeholder = "127.0.0.1"
	inputs[postgresHost].CharLimit = 255
	inputs[postgresHost].Width = 255
	inputs[postgresHost].Prompt = ""
	inputs[postgresHost].Validate = hostValidator
	inputs[postgresHost].SetValue("127.0.0.1")

	inputs[postgresPort] = textinput.New()
	inputs[postgresPort].Placeholder = "5432"
	inputs[postgresPort].CharLimit = 10
	inputs[postgresPort].Width = 15
	inputs[postgresPort].Prompt = ""
	inputs[postgresPort].Validate = portValidator
	inputs[postgresPort].SetValue("5432")

	inputs[postgresUser] = textinput.New()
	inputs[postgresUser].Placeholder = "postgres"
	inputs[postgresUser].CharLimit = 255
	inputs[postgresUser].Width = 30
	inputs[postgresUser].Prompt = ""
	inputs[postgresUser].Validate = userValidator
	inputs[postgresUser].SetValue("postgres")

	inputs[postgresPassword] = textinput.New()
	inputs[postgresPassword].Placeholder = "password"
	inputs[postgresPassword].CharLimit = 255
	inputs[postgresPassword].Width = 15
	inputs[postgresPassword].Prompt = ""
	inputs[postgresPassword].EchoMode = textinput.EchoPassword
	inputs[postgresPassword].Validate = passwordValidator

	inputs[postgresDatabase] = textinput.New()
	inputs[postgresDatabase].Placeholder = "mydb"
	inputs[postgresDatabase].CharLimit = 50
	inputs[postgresDatabase].Width = 15
	inputs[postgresDatabase].Prompt = ""
	inputs[postgresDatabase].Validate = databaseValidator

	inputs[postgresSchema] = textinput.New()
	inputs[postgresSchema].Placeholder = "public"
	inputs[postgresSchema].CharLimit = 50
	inputs[postgresSchema].Width = 30
	inputs[postgresSchema].Prompt = ""
	inputs[postgresSchema].Validate = schemaValidator
	inputs[postgresSchema].SetValue("public")

	return model{
		inputs:  inputs,
		focused: 0,
		err:     nil,
	}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd = make([]tea.Cmd, len(m.inputs))

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			if err := m.validateInput(m.focused); err != nil {
				return m, nil
			}
			if m.focused == len(m.inputs)-1 {
				if err := m.validateAllInputs(); err != nil {
					return m, nil
				}
				m.submitted = true
				return m, tea.Quit
			}
			m.nextInput()
		case tea.KeyCtrlC, tea.KeyEsc:
			m.cancelled = true
			return m, tea.Quit
		case tea.KeyShiftTab, tea.KeyCtrlP:
			m.prevInput()
		case tea.KeyTab, tea.KeyCtrlN:
			if err := m.validateInput(m.focused); err != nil {
				return m, nil
			}
			m.nextInput()
		}
		for i := range m.inputs {
			m.inputs[i].Blur()
		}
		m.inputs[m.focused].Focus()

	// We handle errors just like any other message
	case errMsg:
		m.err = msg
		return m, nil
	}

	for i := range m.inputs {
		m.inputs[i], cmds[i] = m.inputs[i].Update(msg)
	}
	m.err = m.inputs[m.focused].Err
	return m, tea.Batch(cmds...)
}

func (m model) View() string {
	errorMessage := ""
	if m.err != nil {
		errorMessage = inputStyle.Render(m.err.Error())
	}

	return fmt.Sprintf(
		` Start a new session:

 MySQL Configuration:
 %s %s
 %s %s
 %s %s
 %s %s
 %s %s


 PostgreSQL Configuration:
 %s %s
 %s %s
 %s %s
 %s %s
 %s %s
 %s %s

	 %s
	 %s
	`,
		inputStyle.Width(20).Render("MySQL Host"),
		m.inputs[mysqlHost].View(),
		inputStyle.Width(20).Render("MySQL Port"),
		m.inputs[mysqlPort].View(),
		inputStyle.Width(20).Render("MySQL User"),
		m.inputs[mysqlUser].View(),
		inputStyle.Width(20).Render("MySQL Password"),
		m.inputs[mysqlPassword].View(),
		inputStyle.Width(20).Render("MySQL Database"),
		m.inputs[mysqlDatabase].View(),
		inputStyle.Width(20).Render("PostgreSQL Host"),
		m.inputs[postgresHost].View(),
		inputStyle.Width(20).Render("PostgreSQL Port"),
		m.inputs[postgresPort].View(),
		inputStyle.Width(20).Render("PostgreSQL User"),
		m.inputs[postgresUser].View(),
		inputStyle.Width(20).Render("PostgreSQL Password"),
		m.inputs[postgresPassword].View(),
		inputStyle.Width(20).Render("PostgreSQL Database"),
		m.inputs[postgresDatabase].View(),
		inputStyle.Width(20).Render("PostgreSQL Schema"),
		m.inputs[postgresSchema].View(),
		continueStyle.Render("Continue ->"),
		errorMessage,
	) + "\n"
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
			for inputIndex := range m.inputs {
				m.inputs[inputIndex].Blur()
			}
			m.inputs[index].Focus()
			return err
		}
	}
	return nil
}

// nextInput focuses the next input field
func (m *model) nextInput() {
	m.focused = (m.focused + 1) % len(m.inputs)
}

// prevInput focuses the previous input field
func (m *model) prevInput() {
	m.focused--
	// Wrap around
	if m.focused < 0 {
		m.focused = len(m.inputs) - 1
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
