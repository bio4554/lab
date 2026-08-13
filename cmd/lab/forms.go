package main

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Modal create/retire dialogs (mockups 2a/2b): labeled fields, tab to
// move, ←/→ on pickers, enter submits, esc cancels.

type formKind int

const (
	formProject formKind = iota
	formAgent
	formRetire
)

// field is one form row: exactly one of input, area, or pick is used.
type field struct {
	label string
	input *textinput.Model
	area  *textarea.Model
	pick  []string
	pickI int
}

func (f *field) value() string {
	switch {
	case f.input != nil:
		return strings.TrimSpace(f.input.Value())
	case f.area != nil:
		return strings.TrimSpace(f.area.Value())
	case len(f.pick) > 0:
		return f.pick[f.pickI]
	}
	return ""
}

func (f *field) setFocus(on bool) {
	switch {
	case f.input != nil:
		if on {
			f.input.Focus()
		} else {
			f.input.Blur()
		}
	case f.area != nil:
		if on {
			f.area.Focus()
		} else {
			f.area.Blur()
		}
	}
}

type form struct {
	kind    formKind
	title   string
	project string // agent/retire context
	agent   string // retire context
	fields  []*field
	focus   int
	err     string // validation error shown in the dialog
}

func textField(label, placeholder string) *field {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.Prompt = ""
	ti.CharLimit = 512
	return &field{label: label, input: &ti}
}

func areaField(label, placeholder string) *field {
	ta := textarea.New()
	ta.Placeholder = placeholder
	ta.SetHeight(4)
	ta.SetWidth(46)
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	return &field{label: label, area: &ta}
}

func pickField(label string, options []string) *field {
	return &field{label: label, pick: options}
}

func newProjectForm(stacks []string) *form {
	if len(stacks) == 0 {
		stacks = []string{"base"}
	}
	f := &form{
		kind:  formProject,
		title: "NEW PROJECT",
		fields: []*field{
			textField("name", "billing-svc"),
			textField("origin", "path or git URL"),
			pickField("stack", stacks),
		},
	}
	f.fields[0].setFocus(true)
	return f
}

func newAgentForm(project string, stacks []string) *form {
	f := &form{
		kind:    formAgent,
		title:   "NEW AGENT · " + project,
		project: project,
		fields: []*field{
			textField("name", "coder"),
			areaField("role prompt", "You are …"),
			textField("model", "(daemon default)"),
			pickField("credential", []string{"oauth_token", "api_key", "none"}),
		},
	}
	f.fields[0].setFocus(true)
	return f
}

func newRetireForm(project, agent string) *form {
	f := &form{
		kind:    formRetire,
		title:   "RETIRE SESSION · " + agent,
		project: project,
		agent:   agent,
		fields: []*field{
			textField("reason", "context exhausted"),
			areaField("seed prompt", "first prompt of the fresh session"),
		},
	}
	f.fields[0].setFocus(true)
	return f
}

// update handles one key. done is true when the form is finished:
// submitted (submit true) or cancelled.
func (f *form) update(msg tea.KeyMsg) (done, submit bool, cmd tea.Cmd) {
	cur := f.fields[f.focus]
	switch msg.Type {
	case tea.KeyEsc:
		return true, false, nil
	case tea.KeyEnter:
		// Enter inside the role/seed textarea inserts a newline;
		// everywhere else it submits.
		if cur.area == nil {
			if err := f.validate(); err != "" {
				f.err = err
				return false, false, nil
			}
			return true, true, nil
		}
	case tea.KeyTab, tea.KeyDown:
		f.moveFocus(1)
		return false, false, nil
	case tea.KeyShiftTab, tea.KeyUp:
		f.moveFocus(-1)
		return false, false, nil
	case tea.KeyLeft, tea.KeyRight:
		if len(cur.pick) > 0 {
			delta := 1
			if msg.Type == tea.KeyLeft {
				delta = len(cur.pick) - 1
			}
			cur.pickI = (cur.pickI + delta) % len(cur.pick)
			return false, false, nil
		}
	}
	switch {
	case cur.input != nil:
		var c tea.Cmd
		*cur.input, c = cur.input.Update(msg)
		return false, false, c
	case cur.area != nil:
		var c tea.Cmd
		*cur.area, c = cur.area.Update(msg)
		return false, false, c
	}
	return false, false, nil
}

func (f *form) moveFocus(delta int) {
	f.fields[f.focus].setFocus(false)
	f.focus = (f.focus + delta + len(f.fields)) % len(f.fields)
	f.fields[f.focus].setFocus(true)
}

func (f *form) validate() string {
	switch f.kind {
	case formProject:
		if f.fields[0].value() == "" || f.fields[1].value() == "" {
			return "name and origin are required"
		}
	case formAgent:
		if f.fields[0].value() == "" {
			return "name is required"
		}
	}
	return ""
}

// render draws the dialog box.
func (f *form) render() string {
	var rows []string
	rows = append(rows, sTitle.Render(f.title))
	for i, fl := range f.fields {
		label := sDim.Width(12).Render(fl.label)
		var val string
		switch {
		case fl.input != nil:
			val = fl.input.View()
		case fl.area != nil:
			val = fl.area.View()
		default:
			var opts []string
			for j, o := range fl.pick {
				st := sDim
				if j == fl.pickI {
					st = sTabOn
				}
				opts = append(opts, st.Render(o))
			}
			val = strings.Join(opts, sFaint.Render(" │ "))
		}
		box := lipgloss.NewStyle()
		if i == f.focus {
			box = box.Bold(true)
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, label, box.Render(val)))
	}
	if f.err != "" {
		rows = append(rows, sBad.Render(f.err))
	}
	hint := "tab next field · enter " + map[formKind]string{formProject: "create", formAgent: "create", formRetire: "retire"}[f.kind] + " · esc cancel"
	if f.hasArea() {
		hint = "tab next field · enter submits (newline inside prompt) · esc cancel"
	}
	rows = append(rows, sFaint.Render(hint))
	return sModal.Render(strings.Join(rows, "\n"))
}

func (f *form) hasArea() bool {
	for _, fl := range f.fields {
		if fl.area != nil {
			return true
		}
	}
	return false
}
