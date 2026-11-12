// Package reviewtui is the interactive terminal front end for branch review.
//
// # Responsibility
//
// It owns every Charm import so runtime packages stay free of them. Data and decisions come
// from a Backend the caller supplies, so every verdict goes through the same code path as the
// review CLI.
//
// # Allowed dependencies
//
// Unnumbered: it sits outside the numbered layer table and was not changed by the package
// split. May import only lower layers; today it imports textsafe. Third-party:
// charm.land/bubbles/v2/help, charm.land/bubbles/v2/key, charm.land/bubbles/v2/list,
// charm.land/bubbles/v2/textinput, charm.land/bubbles/v2/viewport, charm.land/bubbletea/v2,
// charm.land/lipgloss/v2, github.com/charmbracelet/x/term.
//
// # Invariants
//
//   - Only this package (and its tests) imports Charm libraries.
//   - Run returns ErrNotTerminal rather than starting without an interactive terminal on stdin
//     and stdout.
package reviewtui
