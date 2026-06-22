package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// WatchCondition is the recursive watch DSL. It is a union discriminated by the
// present key. Each leaf/combinator is modelled as a pointer field; exactly one
// is non-nil for a valid condition.
//
// Degenerate conditions are rejected at decode time — a watcher built from one
// could never fire, producing false supervision (the human believes a terminal
// is being watched when it is not). Preserve every guard.
type WatchCondition struct {
	// Leaves
	StateIs         *AgentState `json:"stateIs,omitempty"`
	RuntimeStatusIs *string     `json:"runtimeStatusIs,omitempty"` // "running" | "exited"
	Contains        *string     `json:"contains,omitempty"`
	Regex           *string     `json:"regex,omitempty"`
	NoOutputForMs   *int64      `json:"noOutputForMs,omitempty"`
	ModelJudge      *string     `json:"modelJudge,omitempty"`

	// Combinators
	All []WatchCondition `json:"all,omitempty"`
	Any []WatchCondition `json:"any,omitempty"`
	Not *WatchCondition  `json:"not,omitempty"`
}

// IsComposite reports whether the condition is a combinator (all/any/not).
func (c *WatchCondition) IsComposite() bool {
	return c != nil && (len(c.All) > 0 || len(c.Any) > 0 || c.Not != nil)
}

// rawWatchCondition mirrors WatchCondition without the custom unmarshaller so we
// can decode without infinite recursion.
type rawWatchCondition struct {
	StateIs         *AgentState      `json:"stateIs"`
	RuntimeStatusIs *string          `json:"runtimeStatusIs"`
	Contains        *string          `json:"contains"`
	Regex           *string          `json:"regex"`
	NoOutputForMs   *int64           `json:"noOutputForMs"`
	ModelJudge      *string          `json:"modelJudge"`
	All             []WatchCondition `json:"all"`
	Any             []WatchCondition `json:"any"`
	Not             *WatchCondition  `json:"not"`
}

// UnmarshalJSON decodes a WatchCondition and validates the discriminated-union
// guards. We dispatch on the present key and reject degenerate conditions.
func (c *WatchCondition) UnmarshalJSON(data []byte) error {
	var raw rawWatchCondition
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	*c = WatchCondition{
		StateIs:         raw.StateIs,
		RuntimeStatusIs: raw.RuntimeStatusIs,
		Contains:        raw.Contains,
		Regex:           raw.Regex,
		NoOutputForMs:   raw.NoOutputForMs,
		ModelJudge:      raw.ModelJudge,
		All:             raw.All,
		Any:             raw.Any,
		Not:             raw.Not,
	}
	return c.Validate()
}

// Validate enforces the per-variant guards. A WatchCondition must have exactly
// one present key, and that key must satisfy its guard.
func (c *WatchCondition) Validate() error {
	present := 0
	if c.StateIs != nil {
		present++
		if !c.StateIs.IsValid() {
			return fmt.Errorf("watch condition: invalid stateIs %q", *c.StateIs)
		}
	}
	if c.RuntimeStatusIs != nil {
		present++
		if *c.RuntimeStatusIs != "running" && *c.RuntimeStatusIs != "exited" {
			return fmt.Errorf("watch condition: runtimeStatusIs must be running|exited, got %q", *c.RuntimeStatusIs)
		}
	}
	if c.Contains != nil {
		present++
		// Guard: non-empty after trim. A blank "contains" matches everything,
		// which would fire the watcher immediately on any output.
		if strings.TrimSpace(*c.Contains) == "" {
			return errors.New("watch condition: contains must be non-empty")
		}
	}
	if c.Regex != nil {
		present++
		// Guard: min length 1 AND must compile — an invalid regex would silently
		// never match.
		if len(*c.Regex) < 1 {
			return errors.New("watch condition: regex must be non-empty")
		}
		if _, err := regexp.Compile(*c.Regex); err != nil {
			return fmt.Errorf("watch condition: regex does not compile: %w", err)
		}
	}
	if c.NoOutputForMs != nil {
		present++
		// Guard: positive int. A non-positive (or Infinity, which would persist
		// as null → compared as 0) value fires the timeout immediately.
		if *c.NoOutputForMs <= 0 {
			return errors.New("watch condition: noOutputForMs must be a positive integer")
		}
	}
	if c.ModelJudge != nil {
		present++
		if strings.TrimSpace(*c.ModelJudge) == "" {
			return errors.New("watch condition: modelJudge must be non-empty")
		}
	}
	if c.All != nil {
		present++
		if len(c.All) < 1 {
			return errors.New("watch condition: all must have at least one member")
		}
		for i := range c.All {
			if err := c.All[i].Validate(); err != nil {
				return err
			}
		}
	}
	if c.Any != nil {
		present++
		if len(c.Any) < 1 {
			return errors.New("watch condition: any must have at least one member")
		}
		for i := range c.Any {
			if err := c.Any[i].Validate(); err != nil {
				return err
			}
		}
	}
	if c.Not != nil {
		present++
		if err := c.Not.Validate(); err != nil {
			return err
		}
	}

	if present == 0 {
		return errors.New("watch condition: no variant key present")
	}
	if present > 1 {
		return errors.New("watch condition: exactly one variant key allowed")
	}
	return nil
}
