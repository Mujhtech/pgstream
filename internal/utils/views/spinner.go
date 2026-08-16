package views

import (
	"errors"
	"fmt"
	"os"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

var ErrCancelled = errors.New("operation cancelled")

type model struct {
	spinner  spinner.Model
	quitting bool
	aborted  bool
	message  string
	inline   bool
}

type runResult struct {
	model tea.Model
	err   error
}

type spinnerRun struct {
	program *tea.Program
	done    <-chan runResult
}

type Msg string

func initialModel(message string, inline bool) model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(Primary)
	return model{spinner: s, message: message, inline: inline}
}

func (m model) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case Msg:
		m.quitting = true
		return m, tea.Quit

	case tea.KeyMsg:
		switch keypress := msg.String(); keypress {
		case "ctrl+c":
			m.aborted = true
			m.quitting = true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd

}

// stdoutIsTerminal reports whether an animated spinner can render; without a
// TTY (CI, piped output, nohup) the spinner degrades to one printed line.
func stdoutIsTerminal() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
}

func WithSpinner(message string, fn func() error) (err error) {
	if !stdoutIsTerminal() {
		fmt.Println(message)
		return fn()
	}
	runner := start(message, false)
	defer func() {
		err = errors.Join(err, runner.stop())
	}()
	return fn()
}

func WithInlineSpinner(message string, fn func() error) (err error) {
	if !stdoutIsTerminal() {
		fmt.Println(message)
		return fn()
	}
	runner := start(message, true)
	defer func() {
		err = errors.Join(err, runner.stop())
	}()
	return fn()
}

func start(message string, inline bool) *spinnerRun {
	var p *tea.Program
	if inline {
		p = tea.NewProgram(initialModel(message, true))
	} else {
		p = tea.NewProgram(initialModel(message, false), tea.WithAltScreen())
	}
	done := make(chan runResult, 1)
	go func() {
		finalModel, err := p.Run()
		done <- runResult{model: finalModel, err: err}
	}()
	return &spinnerRun{program: p, done: done}
}

func (r *spinnerRun) stop() error {
	r.program.Send(Msg("quit"))
	result := <-r.done
	if result.err != nil {
		return fmt.Errorf("run progress display: %w", result.err)
	}
	finalModel, ok := result.model.(model)
	if ok && finalModel.aborted {
		return ErrCancelled
	}
	return nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}

	str := ""
	if m.inline {
		str = GetInfoMessage(fmt.Sprintf("%s %s", m.spinner.View(), m.message))
	} else {
		str = DocStyle.Render(fmt.Sprintf("\n\n   %s %s\n\n", m.spinner.View(), m.message))
	}

	return str
}
